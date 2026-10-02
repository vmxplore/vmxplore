package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseBackupDest(t *testing.T) {
	good := map[string]backupDest{
		"tank/backups":                 {"", "tank/backups"},
		" tank/backups/ ":              {"", "tank/backups"},
		"root@nas:tank/backups":        {"root@nas", "tank/backups"},
		"nas.lan:pool/vm-backups/onyx": {"nas.lan", "pool/vm-backups/onyx"},
	}
	for in, want := range good {
		got, err := parseBackupDest(in)
		if err != nil || got != want {
			t.Errorf("parseBackupDest(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "tank/back ups", "tank/x;reboot", "a b@host:tank/x",
		"host:", "-oProxyCommand=x:tank/x", "tank/$(id)", "tank/../etc"} {
		if _, err := parseBackupDest(bad); err == nil {
			t.Errorf("parseBackupDest(%q) accepted a bad destination", bad)
		}
	}
}

func TestRestoredDiskName(t *testing.T) {
	cases := [][4]string{
		{"app-plex-on-zf", "app-plex-on-zf", "plex2", "plex2"},
		{"app-plex-on-zf-data", "app-plex-on-zf", "plex2", "plex2-data"},
		{"scratch", "app-plex-on-zf", "plex2", "plex2-scratch"},
	}
	for _, c := range cases {
		if got := restoredDiskName(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("restoredDiskName(%q,%q,%q) = %q, want %q", c[0], c[1], c[2], got, c[3])
		}
	}
}

const backupSampleXML = `<domain type='kvm'>
  <name>web</name>
  <uuid>0b1c2d3e-0000-4000-8000-000000000001</uuid>
  <os><nvram>/var/lib/libvirt/qemu/nvram/web_VARS.fd</nvram></os>
  <devices>
    <disk type='block' device='disk'><source dev='/dev/zvol/rpool/vms/web'/><target dev='vda'/></disk>
    <disk type='block' device='disk'><source dev='/dev/zvol/rpool/vms/web-data'/><target dev='vdb'/></disk>
    <interface type='bridge'><mac address='52:54:00:aa:bb:cc'/><source bridge='br0'/></interface>
    <channel type='unix'><target type='virtio' name='org.qemu.guest_agent.0'/><name>not-the-domain</name></channel>
  </devices>
</domain>`

func TestRewriteDomainXML(t *testing.T) {
	m := map[string]string{
		originalDiskPath(backupSampleXML, "web"):      "rpool/vms/web2",
		originalDiskPath(backupSampleXML, "web-data"): "rpool/vms/web2-data",
	}
	if m["rpool/vms/web"] == "" || m["rpool/vms/web-data"] == "" {
		t.Fatalf("originalDiskPath did not find both disks: %v", m)
	}
	x := rewriteDomainXML(backupSampleXML, "web", "web2", m)
	for _, want := range []string{"<name>web2</name>", "/dev/zvol/rpool/vms/web2'", "/dev/zvol/rpool/vms/web2-data'",
		"nvram/web2_VARS.fd", "<name>not-the-domain</name>"} {
		if !strings.Contains(x, want) {
			t.Errorf("rewritten XML lacks %q", want)
		}
	}
	for _, gone := range []string{"<uuid>", "<mac address", "/dev/zvol/rpool/vms/web'", "/dev/zvol/rpool/vms/web-data'"} {
		if strings.Contains(x, gone) {
			t.Errorf("rewritten XML still has %q", gone)
		}
	}
}

// TestBackupRestoreZFS runs the real thing against a throwaway file-backed
// pool. Opt-in (VMX_ZFS_IT=1): it needs zfs and sudo, and it creates and
// destroys the pool "vmxbktest" -- that exact name, nothing else.
func TestBackupRestoreZFS(t *testing.T) {
	if os.Getenv("VMX_ZFS_IT") != "1" {
		t.Skip("set VMX_ZFS_IT=1 to run against a throwaway pool (needs zfs + sudo)")
	}
	ctx := context.Background()
	sudo := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	const pool = "vmxbktest"
	// not t.TempDir: the pool's mount directories are root's, and the test
	// framework cannot remove them as the operator; this exact path is
	// removed with sudo in the cleanup below
	alt := filepath.Join(os.Getenv("HOME"), ".cache", "vmxbktest-alt")
	vdev := filepath.Join(os.Getenv("HOME"), ".cache", "vmxbktest.img")
	_ = exec.Command("sudo", "-n", "zpool", "destroy", "-f", pool).Run() // a leftover from a killed run
	sudo("truncate", "-s", "1G", vdev)
	sudo("mkdir", "-p", alt)
	sudo("zpool", "create", "-R", alt, "-O", "mountpoint=/"+pool, pool, vdev)
	t.Cleanup(func() {
		_ = exec.Command("sudo", "-n", "zpool", "destroy", "-f", pool).Run()
		_ = exec.Command("sudo", "-n", "rm", "-f", vdev).Run()
		_ = exec.Command("sudo", "-n", "rm", "-rf", "--one-file-system", alt).Run()
	})
	sudo("zfs", "create", "-p", pool+"/vms")
	sudo("zfs", "create", "-V", "16M", pool+"/vms/web")
	sudo("zfs", "create", "-V", "8M", pool+"/vms/web-data")
	sudo("zfs", "create", "-p", pool+"/bk")
	dev := func(ds string) string { return "/dev/zvol/" + ds }
	write := func(ds, data string) {
		sudo("udevadm", "settle")
		cmd := exec.Command("sudo", "-n", "dd", "of="+dev(ds), "bs=4k", "conv=notrunc,fsync", "status=none")
		cmd.Stdin = strings.NewReader(data)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("write %s: %v %s", ds, err, out)
		}
	}
	read := func(ds string, n int) string {
		sudo("udevadm", "settle")
		out, err := exec.Command("sudo", "-n", "head", "-c", itoa(n), dev(ds)).Output()
		if err != nil {
			t.Fatalf("read %s: %v", ds, err)
		}
		return string(bytes.TrimRight(out, "\x00"))
	}
	xml := strings.ReplaceAll(strings.ReplaceAll(backupSampleXML, "rpool/vms", pool+"/vms"), "<name>web</name>", "<name>web</name>")
	dest := backupDest{Dataset: pool + "/bk"}
	var logs []string
	log := func(s string) { logs = append(logs, s) }

	write(pool+"/vms/web", "first version of web")
	write(pool+"/vms/web-data", "data one")
	b1, err := BackupDisks(ctx, "web", []string{pool + "/vms/web", pool + "/vms/web-data"}, xml, dest, 2, log)
	if err != nil {
		t.Fatalf("first backup: %v\n%s", err, strings.Join(logs, "\n"))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "(full)") {
		t.Errorf("first backup was not a full send: %v", logs)
	}

	write(pool+"/vms/web", "second version of web")
	logs = nil
	sleepSecond() // backup names have one-second resolution
	b2, err := BackupDisks(ctx, "web", []string{pool + "/vms/web", pool + "/vms/web-data"}, xml, dest, 2, log)
	if err != nil {
		t.Fatalf("second backup: %v\n%s", err, strings.Join(logs, "\n"))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "incremental from "+b1) {
		t.Errorf("second backup was not incremental from %s: %v", b1, logs)
	}
	sleepSecond()
	if _, err := BackupDisks(ctx, "web", []string{pool + "/vms/web", pool + "/vms/web-data"}, xml, dest, 2, log); err != nil {
		t.Fatalf("third backup: %v", err)
	}
	sets, err := ListBackups(ctx, dest)
	if err != nil || len(sets) != 1 || sets[0].VM != "web" || len(sets[0].Points) != 2 {
		t.Fatalf("ListBackups = %+v, %v; want web with 2 points (keep=2)", sets, err)
	}
	// the VM keeps only the newest backup snapshot
	if src, _ := backupSnaps(ctx, srcZFS, pool+"/vms/web"); len(src) != 1 {
		t.Errorf("source keeps %v backup snapshots, want only the newest", src)
	}

	// restore the SECOND point as a new VM; its data is the second version
	pts := sets[0].Points // newest first: [b3, b2]
	if pts[1] != b2 {
		t.Fatalf("points %v, want %s second", pts, b2)
	}
	logs = nil
	newXML, made, err := RestoreDisks(ctx, dest, "web", b2, pool+"/vms", "web2", log)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, strings.Join(logs, "\n"))
	}
	if len(made) != 2 {
		t.Errorf("restore made %v, want two disks", made)
	}
	if got := read(pool+"/vms/web2", 21); got != "second version of web" {
		t.Errorf("restored web2 holds %q, want the second version", got)
	}
	if got := read(pool+"/vms/web2-data", 8); got != "data one" {
		t.Errorf("restored web2-data holds %q", got)
	}
	if !strings.Contains(newXML, "<name>web2</name>") || !strings.Contains(newXML, dev(pool+"/vms/web2-data")) || strings.Contains(newXML, "<uuid>") {
		t.Errorf("restored definition not rewritten:\n%s", newXML)
	}

	// restoring onto a name whose disks exist refuses and leaves them alone
	if _, _, err := RestoreDisks(ctx, dest, "web", b2, pool+"/vms", "web2", log); err == nil {
		t.Fatal("a second restore onto existing disks succeeded; it must refuse")
	}
	if got := read(pool+"/vms/web2", 21); got != "second version of web" {
		t.Errorf("the refused restore changed web2: %q", got)
	}

	// restored disks start clean (no backup-* snapshot carried over)
	for _, m := range made {
		if snaps, _ := backupSnaps(ctx, srcZFS, m); len(snaps) != 0 {
			t.Errorf("restored %s carries %v", m, snaps)
		}
	}

	// a restore that fails part-way removes the disks it made, and only those
	sudo("zfs", "create", "-V", "8M", pool+"/vms/web3-data") // collides with the second disk
	if _, _, err := RestoreDisks(ctx, dest, "web", b2, pool+"/vms", "web3", log); err == nil {
		t.Fatal("restore onto an existing second disk succeeded")
	}
	if exists(ctx, srcZFS, pool+"/vms/web3") {
		t.Error("the failed restore left web3 (the disk it made) behind")
	}
	if !exists(ctx, srcZFS, pool+"/vms/web3-data") {
		t.Error("the failed restore removed web3-data, which it did not make")
	}

	// a destination that lost its shared snapshot is not overwritten
	sudo("zfs", "destroy", pool+"/vms/web@"+pts[0]) // the VM's only base
	if _, err := BackupDisks(ctx, "web", []string{pool + "/vms/web", pool + "/vms/web-data"}, xml, dest, 2, log); err == nil ||
		!strings.Contains(err.Error(), "nothing is overwritten") {
		t.Fatalf("backup over a diverged destination: err = %v; want a refusal", err)
	}
	if sets2, _ := ListBackups(ctx, dest); len(sets2) != 1 || len(sets2[0].Points) != 2 {
		t.Errorf("the refused backup changed the destination: %+v", sets2)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func sleepSecond() { time.Sleep(1100 * time.Millisecond) }

// The definition round-trips through the chunked properties, and every
// value stays under ZFS's 8192-byte limit even for a large definition.
func TestDomXMLEncodingRoundTrips(t *testing.T) {
	big := strings.Repeat(backupSampleXML, 40) // ~20 KB, past one chunk even gzipped? check below
	big += strings.Repeat("x", 30000)          // incompressible-ish tail is not needed; length matters
	props, err := encodeDomXML(big)
	if err != nil {
		t.Fatal(err)
	}
	var enc strings.Builder
	n := -1
	for _, p := range props {
		k, v, _ := strings.Cut(p, "=")
		if len(v) > 8000 {
			t.Errorf("%s is %d bytes, over the property limit", k, len(v))
		}
		if k == domXMLProp+".n" {
			n, _ = strconv.Atoi(v)
			continue
		}
		enc.WriteString(v)
	}
	if n != len(props)-1 {
		t.Fatalf(".n = %d for %d chunks", n, len(props)-1)
	}
	raw, err := base64.StdEncoding.DecodeString(enc.String())
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != big {
		t.Fatalf("round trip lost data: %d bytes in, %d out", len(big), len(got))
	}
}
