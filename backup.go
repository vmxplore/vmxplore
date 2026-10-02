// backup.go — back up a VM's ZFS disks (and its libvirt definition) to a
// dataset on this host or another, and restore one as a new VM.
//
// What a backup is, in order:
//  1. every zvol behind the VM's disks is snapshotted together,
//     @backup-YYYYMMDD-HHMMSS (UTC);
//  2. each is sent to <dest>/<vm>/<disk> -- in full the first time,
//     incrementally from the newest backup both sides share after that;
//  3. <dest>/<vm> is snapshotted with the same name and the domain XML
//     (virsh dumpxml --inactive) is stored ON that snapshot, as user
//     properties vmxplore:domxml.N (gzip+base64, chunked under ZFS's 8 KB
//     value limit), so every backup point carries the definition that
//     matches it and no mounted filesystem is needed anywhere: kldload's
//     root pool is mountpoint=none, and a domain.xml file had nowhere to
//     go (first run against a real VM on onyx, 2026-10-01);
//  4. the destination keeps the newest `keep` backups; the VM keeps only
//     the newest backup snapshot, as the base for the next incremental.
//
// Why: an enthusiast wants "back up this VM" and "restore it" as buttons
// (operator, 2026-10-01), and ZFS makes both cheap and exact. The TUI's
// replicate verb sends one snapshot with `recv -F`, which rolls the
// destination back; a backup must never destroy what it is protecting, so
// nothing here receives with -F. A destination that has diverged from the
// VM's history stops the backup with an error instead.
//
// Restore never overwrites: it receives into NEW zvols for a NEW domain
// name, gives it a fresh UUID and fresh MACs (libvirt makes them), points
// its disks at the new zvols, and defines it without starting it. Restoring
// over a VM that still exists is a delete followed by a restore, done on
// purpose.
//
// Where: the source is the hypervisor vmxplore manages (zfsArgv: local, or
// over ssh for a remote target); the destination is "pool/ds" on this host
// or "user@host:pool/ds" on another. Send and receive are two processes
// joined by a pipe here, so any pairing of local and remote works.
//
// Names: only backup-<UTC timestamp> snapshots are ever destroyed, and only
// on the datasets this backup made; nothing else is touched (destroy by
// exact name, never a pattern).
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// backupDest is where backups live: Dataset on Host ("" = this machine).
type backupDest struct {
	Host    string
	Dataset string
}

func (d backupDest) String() string {
	if d.Host == "" {
		return d.Dataset
	}
	return d.Host + ":" + d.Dataset
}

var (
	backupSnapRe = regexp.MustCompile(`^backup-[0-9]{8}-[0-9]{6}$`)
	hostRe       = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9._-]+$`)
)

// parseBackupDest reads "pool/ds" or "user@host:pool/ds". Every dataset
// component must pass validZFSName (these reach zfs argv, and over ssh a
// remote shell); the host is a plain user@host.
func parseBackupDest(s string) (backupDest, error) {
	s = strings.TrimSpace(s)
	var d backupDest
	if host, ds, ok := strings.Cut(s, ":"); ok {
		d.Host, d.Dataset = host, ds
		if !hostRe.MatchString(host) {
			return d, fmt.Errorf("destination host %q: want host or user@host", host)
		}
	} else {
		d.Dataset = s
	}
	d.Dataset = strings.Trim(d.Dataset, "/")
	if d.Dataset == "" {
		return d, errors.New("destination: a dataset such as tank/backups, or user@host:tank/backups")
	}
	for _, c := range strings.Split(d.Dataset, "/") {
		if c == "." || c == ".." {
			// the allowlist admits dots, so ".." passed it (caught by this
			// file's own hostile-input test)
			return d, fmt.Errorf("destination dataset %q: no . or .. components", d.Dataset)
		}
		if err := validZFSName(c); err != nil {
			return d, fmt.Errorf("destination dataset %q: %w", d.Dataset, err)
		}
	}
	return d, nil
}

// ── who runs what ───────────────────────────────────────────────────────────

// srcZFS is zfs on the hypervisor, as root.
func srcZFS(args ...string) []string {
	argv := zfsArgv(args...)
	if target.SSHHost == "" && os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	return argv
}

// dstArgv runs argv where the backups live, as root locally.
func (d backupDest) argv(args ...string) []string {
	if d.Host != "" {
		return sshArgv(d.Host, args...)
	}
	if os.Geteuid() != 0 {
		return append([]string{"sudo", "-n"}, args...)
	}
	return args
}

func (d backupDest) zfs(args ...string) []string {
	return d.argv(append([]string{"zfs"}, args...)...)
}

// run executes argv with optional stdin and returns trimmed stdout; stderr
// rides the error so a failure names its cause.
func run(ctx context.Context, stdin io.Reader, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	rc := 0
	if err != nil {
		rc = 1
	}
	auditLog(strings.Join(argv, " "), rc)
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", argv[len(argv)-1], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// pipe runs send | recv and fails if either side does.
func pipe(ctx context.Context, send, recv []string) error {
	s := exec.CommandContext(ctx, send[0], send[1:]...)
	r := exec.CommandContext(ctx, recv[0], recv[1:]...)
	var serr, rerr bytes.Buffer
	s.Stderr, r.Stderr = &serr, &rerr
	pr, pw := io.Pipe()
	s.Stdout, r.Stdin = pw, pr
	if err := r.Start(); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		pw.Close()
		_ = r.Wait() // error ignored: the start failure is the one reported
		return err
	}
	sendErr := s.Wait()
	pw.Close()
	recvErr := r.Wait()
	auditLog(strings.Join(send, " ")+" | "+strings.Join(recv, " "), map[bool]int{true: 1, false: 0}[sendErr != nil || recvErr != nil])
	switch {
	case recvErr != nil:
		return fmt.Errorf("receive: %s", stderrLine(rerr.String(), recvErr))
	case sendErr != nil:
		return fmt.Errorf("send: %s", stderrLine(serr.String(), sendErr))
	}
	return nil
}

func stderrLine(s string, err error) string {
	if s = strings.TrimSpace(s); s != "" {
		return strings.SplitN(s, "\n", 2)[0]
	}
	return err.Error()
}

// backupSnaps lists ds's backup-* snapshot names (no "ds@"), oldest first.
// A dataset that does not exist has none.
func backupSnaps(ctx context.Context, zfs func(...string) []string, ds string) ([]string, error) {
	out, err := run(ctx, nil, zfs("list", "-H", "-t", "snapshot", "-o", "name", "-s", "createtxg", "-d", "1", ds))
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if _, s, ok := strings.Cut(strings.TrimSpace(l), "@"); ok && backupSnapRe.MatchString(s) {
			names = append(names, s)
		}
	}
	return names, nil
}

func exists(ctx context.Context, zfs func(...string) []string, ds string) bool {
	_, err := run(ctx, nil, zfs("list", "-H", "-o", "name", ds))
	return err == nil
}

// vmZvols is every zvol behind r's disks, in disk order.
func vmZvols(r Row) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range r.D.Disks {
		if ds := zvolDataset(d.Dev); ds != "" && !seen[ds] {
			seen[ds] = true
			out = append(out, ds)
		}
	}
	if len(out) == 0 && r.DS != nil && r.DS.Type == "volume" {
		out = append(out, r.DS.Name)
	}
	return out
}

func base(ds string) string { return ds[strings.LastIndex(ds, "/")+1:] }

// ── backup ──────────────────────────────────────────────────────────────────

// BackupDisks backs up disks (zvols on the hypervisor) for the VM named vm,
// with domXML as its definition, to dest, keeping the newest keep backups
// there. log gets one line per step. Returns the backup name.
func BackupDisks(ctx context.Context, vm string, disks []string, domXML string, dest backupDest, keep int, log func(string)) (string, error) {
	if len(disks) == 0 {
		return "", fmt.Errorf("%s has no ZFS-backed disks to back up", vm)
	}
	if err := validZFSName(vm); err != nil {
		return "", fmt.Errorf("VM name %w", err)
	}
	if keep < 1 {
		keep = 1
	}
	name := "backup-" + time.Now().UTC().Format("20060102-150405")
	parent := dest.Dataset + "/" + vm

	// 1. all disks at one instant (one zfs call is atomic within a pool)
	snaps := make([]string, len(disks))
	for i, d := range disks {
		snaps[i] = d + "@" + name
	}
	if _, err := run(ctx, nil, srcZFS(append([]string{"snapshot"}, snaps...)...)); err != nil {
		if !strings.Contains(err.Error(), "same pool") {
			return "", fmt.Errorf("snapshot: %w", err)
		}
		for _, s := range snaps { // disks on different pools: one at a time
			if _, err := run(ctx, nil, srcZFS("snapshot", s)); err != nil {
				return "", fmt.Errorf("snapshot: %w", err)
			}
		}
	}
	log(fmt.Sprintf("snapshot %s on %d disk(s)", name, len(disks)))
	// fail undoes this run's snapshot on the VM wherever the destination
	// did not receive it -- there it is not a base for anything, and a
	// failed backup left it on the VM to pin space (live run, 2026-10-01).
	fail := func(err error) (string, error) {
		bg := context.Background()
		for _, d := range disks {
			if !exists(bg, dest.zfs, dest.Dataset+"/"+vm+"/"+base(d)+"@"+name) {
				if _, derr := run(bg, nil, srcZFS("destroy", d+"@"+name)); derr != nil {
					log("could not remove " + d + "@" + name + ": " + derr.Error())
				}
			}
		}
		return "", err
	}

	// 2. the destination tree
	if _, err := run(ctx, nil, dest.zfs("create", "-p", parent)); err != nil {
		return fail(fmt.Errorf("destination %s: %w", dest, err))
	}

	// 3. each disk: incremental from the newest shared backup, else full
	for _, d := range disks {
		dd := parent + "/" + base(d)
		have, err := backupSnaps(ctx, dest.zfs, dd)
		if err != nil {
			return fail(err)
		}
		send := []string{"send", d + "@" + name}
		kind := "full"
		if len(have) > 0 {
			last := have[len(have)-1]
			if !exists(ctx, srcZFS, d+"@"+last) {
				return fail(fmt.Errorf("%s has backups on %s but %s no longer has %s, the snapshot they share; "+
					"move that destination dataset aside (nothing is overwritten) and back up again for a new full copy",
					dd, dest, d, last))
			}
			send = []string{"send", "-i", "@" + last, d + "@" + name}
			kind = "incremental from " + last
		} else if exists(ctx, dest.zfs, dd) {
			return fail(fmt.Errorf("%s exists on %s but holds no backup-* snapshot; not overwriting it", dd, dest))
		}
		log(fmt.Sprintf("sending %s (%s)…", base(d), kind))
		t0 := time.Now()
		if err := pipe(ctx, srcZFS(send...), dest.zfs("recv", "-u", "-o", "readonly=on", dd)); err != nil {
			return fail(fmt.Errorf("%s: %w", base(d), err))
		}
		log(fmt.Sprintf("  %s done in %s", base(d), time.Since(t0).Round(time.Second)))
	}

	// 4. the definition, on a snapshot of its own with the same name
	if _, err := run(ctx, nil, dest.zfs("snapshot", parent+"@"+name)); err != nil {
		return fail(fmt.Errorf("snapshot %s: %w", parent, err))
	}
	props, err := encodeDomXML(domXML)
	if err != nil {
		return fail(err)
	}
	if _, err := run(ctx, nil, dest.zfs(append([]string{"set"}, append(props, parent+"@"+name)...)...)); err != nil {
		return fail(fmt.Errorf("storing the definition: %w", err))
	}
	log("definition stored with the backup")

	// 5. retention, by exact name only
	pruned := 0
	for _, dd := range append([]string{parent}, func() []string {
		var out []string
		for _, d := range disks {
			out = append(out, parent+"/"+base(d))
		}
		return out
	}()...) {
		have, err := backupSnaps(ctx, dest.zfs, dd)
		if err != nil {
			return name, err
		}
		for i := 0; i < len(have)-keep; i++ {
			if _, err := run(ctx, nil, dest.zfs("destroy", dd+"@"+have[i])); err != nil {
				log("  could not prune " + dd + "@" + have[i] + ": " + err.Error())
				continue
			}
			pruned++
		}
	}
	for _, d := range disks { // the VM keeps only the newest, as the next base
		have, err := backupSnaps(ctx, srcZFS, d)
		if err != nil {
			continue
		}
		for _, s := range have {
			if s != name {
				if _, err := run(ctx, nil, srcZFS("destroy", d+"@"+s)); err != nil {
					log("  could not remove the old base " + d + "@" + s + ": " + err.Error())
				}
			}
		}
	}
	if pruned > 0 {
		log(fmt.Sprintf("kept the newest %d backup(s); pruned %d old snapshot(s)", keep, pruned))
	}
	return name, nil
}

// ── listing ─────────────────────────────────────────────────────────────────

// BackupSet is one VM's backups at a destination, newest first.
type BackupSet struct {
	VM     string
	Points []string
}

// ListBackups reads the VMs backed up at dest and their backup points.
func ListBackups(ctx context.Context, dest backupDest) ([]BackupSet, error) {
	out, err := run(ctx, nil, dest.zfs("list", "-H", "-o", "name", "-d", "1", "-t", "filesystem", dest.Dataset))
	if err != nil {
		return nil, err
	}
	var sets []BackupSet
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l == dest.Dataset {
			continue
		}
		pts, err := backupSnaps(ctx, dest.zfs, l)
		if err != nil || len(pts) == 0 {
			continue
		}
		sort.Sort(sort.Reverse(sort.StringSlice(pts)))
		sets = append(sets, BackupSet{VM: base(l), Points: pts})
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].VM < sets[j].VM })
	return sets, nil
}

// ── restore ─────────────────────────────────────────────────────────────────

// restoredDiskName maps a backed-up disk to its new zvol name: the VM's
// name at the front of the disk's name is swapped for the new one
// (app-plex-on-zf-data -> plex2-data); any other disk gets the new name in
// front.
func restoredDiskName(diskBase, oldVM, newVM string) string {
	if diskBase == oldVM {
		return newVM
	}
	if rest, ok := strings.CutPrefix(diskBase, oldVM+"-"); ok {
		return newVM + "-" + rest
	}
	return newVM + "-" + diskBase
}

var (
	reName = regexp.MustCompile(`(?s)<name>[^<]*</name>`)
	reUUID = regexp.MustCompile(`\s*<uuid>[^<]*</uuid>`)
	reMAC  = regexp.MustCompile(`\s*<mac address=['"][^'"]*['"]\s*/>`)
)

// rewriteDomainXML gives a backed-up definition its new identity: the new
// name (the first <name>, the domain's), no <uuid> and no <mac> (libvirt
// generates fresh ones, so the copy never collides with the original on the
// network), disk paths pointing at the restored zvols, and the per-VM nvram
// file renamed (libvirt recreates it from the template when it is absent).
func rewriteDomainXML(xml, oldVM, newVM string, diskMap map[string]string) string {
	done := false
	xml = reName.ReplaceAllStringFunc(xml, func(m string) string {
		if done {
			return m
		}
		done = true
		return "<name>" + newVM + "</name>"
	})
	xml = reUUID.ReplaceAllString(xml, "")
	xml = reMAC.ReplaceAllString(xml, "")
	// longest first, so a disk whose name is a prefix of another's cannot
	// rewrite part of the longer path
	olds := make([]string, 0, len(diskMap))
	for o := range diskMap {
		olds = append(olds, o)
	}
	sort.Slice(olds, func(i, j int) bool { return len(olds[i]) > len(olds[j]) })
	for _, o := range olds {
		xml = strings.ReplaceAll(xml, "/dev/zvol/"+o+"'", "/dev/zvol/"+diskMap[o]+"'")
		xml = strings.ReplaceAll(xml, "/dev/zvol/"+o+"\"", "/dev/zvol/"+diskMap[o]+"\"")
	}
	xml = strings.ReplaceAll(xml, "/nvram/"+oldVM+"_VARS", "/nvram/"+newVM+"_VARS")
	return xml
}

// RestoreDisks receives backup point `point` of vm at dest into new zvols
// under parent (on the hypervisor) named for newVM, and returns the
// definition rewritten for them. Nothing existing is overwritten: a target
// zvol that exists stops the restore. On failure, the zvols it made are
// destroyed again (by exact name) and the error says so.
func RestoreDisks(ctx context.Context, dest backupDest, vm, point, parent, newVM string, log func(string)) (string, []string, error) {
	if err := validZFSName(newVM); err != nil {
		return "", nil, fmt.Errorf("new VM name %w", err)
	}
	if !backupSnapRe.MatchString(point) {
		return "", nil, fmt.Errorf("%q is not a backup point", point)
	}
	src := dest.Dataset + "/" + vm
	out, err := run(ctx, nil, dest.zfs("list", "-H", "-o", "name,type", "-d", "1", src))
	if err != nil {
		return "", nil, err
	}
	var disks []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[1] == "volume" {
			disks = append(disks, f[0])
		}
	}
	if len(disks) == 0 {
		return "", nil, fmt.Errorf("%s holds no disks", src)
	}
	xml, err := readDomXML(ctx, dest, src+"@"+point)
	if err != nil {
		return "", nil, fmt.Errorf("the definition stored with %s@%s: %w", src, point, err)
	}

	diskMap := map[string]string{}
	var made []string
	undo := func(cause error) (string, []string, error) {
		for i := len(made) - 1; i >= 0; i-- {
			// -r on a zvol THIS restore made: its only children are the
			// snapshot the receive brought in
			if _, err := run(context.Background(), nil, srcZFS("destroy", "-r", made[i])); err != nil {
				log("  could not remove " + made[i] + ": " + err.Error())
			}
		}
		if len(made) > 0 {
			log(fmt.Sprintf("removed the %d disk(s) this restore had made", len(made)))
		}
		return "", nil, cause
	}
	for _, d := range disks {
		nd := parent + "/" + restoredDiskName(base(d), vm, newVM)
		if exists(ctx, srcZFS, nd) {
			return undo(fmt.Errorf("%s already exists; not overwriting it -- pick another name", nd))
		}
		log(fmt.Sprintf("restoring %s → %s…", base(d), nd))
		t0 := time.Now()
		if err := pipe(ctx, dest.zfs("send", d+"@"+point), srcZFS("recv", "-u", nd)); err != nil {
			return undo(fmt.Errorf("%s: %w", base(d), err))
		}
		made = append(made, nd)
		// the receive brought the backup's snapshot along: the new VM does
		// not need it, and it made the zvol undeletable without -r (a live
		// run's cleanup left the disk behind, 2026-10-01)
		if _, err := run(ctx, nil, srcZFS("destroy", nd+"@"+point)); err != nil {
			log("  note: could not drop " + nd + "@" + point + ": " + err.Error())
		}
		log(fmt.Sprintf("  done in %s", time.Since(t0).Round(time.Second)))
		// the original disk's name, as the definition refers to it, is the
		// one on the hypervisor; recover it from the XML by its base name
		diskMap[originalDiskPath(xml, base(d))] = nd
	}
	delete(diskMap, "")
	return rewriteDomainXML(xml, vm, newVM, diskMap), made, nil
}

// originalDiskPath finds the zvol path in xml whose last component is b
// ("" when absent): the backup stores disks by base name, the definition
// by full path.
func originalDiskPath(xml, b string) string {
	re := regexp.MustCompile(`/dev/zvol/([^'"]*/` + regexp.QuoteMeta(b) + `)['"]`)
	if m := re.FindStringSubmatch(xml); m != nil {
		return m[1]
	}
	return ""
}

// ── the definition, as snapshot properties ──────────────────────────────────

const domXMLProp = "vmxplore:domxml"

// domXMLChunk keeps each property value under ZFS's 8192-byte limit.
const domXMLChunk = 7000

// encodeDomXML returns "vmxplore:domxml.N=..." assignments for zfs set:
// the XML gzipped and base64'd (a 6 KB definition is ~2 KB), split into
// numbered chunks, plus .n with the count.
func encodeDomXML(xml string) ([]string, error) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(xml)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(b.Bytes())
	var out []string
	n := 0
	for len(enc) > 0 {
		c := enc
		if len(c) > domXMLChunk {
			c = c[:domXMLChunk]
		}
		out = append(out, fmt.Sprintf("%s.%d=%s", domXMLProp, n, c))
		enc = enc[len(c):]
		n++
	}
	return append(out, fmt.Sprintf("%s.n=%d", domXMLProp, n)), nil
}

// readDomXML reassembles the definition stored on snap by encodeDomXML.
func readDomXML(ctx context.Context, dest backupDest, snap string) (string, error) {
	cnt, err := run(ctx, nil, dest.zfs("get", "-H", "-o", "value", domXMLProp+".n", snap))
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(cnt)
	if err != nil || n < 1 {
		return "", fmt.Errorf("no definition stored (%s.n = %q)", domXMLProp, cnt)
	}
	var enc strings.Builder
	for i := 0; i < n; i++ {
		v, err := run(ctx, nil, dest.zfs("get", "-H", "-o", "value", fmt.Sprintf("%s.%d", domXMLProp, i), snap))
		if err != nil {
			return "", err
		}
		enc.WriteString(v)
	}
	raw, err := base64.StdEncoding.DecodeString(enc.String())
	if err != nil {
		return "", fmt.Errorf("stored definition is corrupt: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("stored definition is corrupt: %w", err)
	}
	xml, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("stored definition is corrupt: %w", err)
	}
	return string(xml), nil
}
