// verbs.go — the 0.2 mutation verbs: what they run, and the gates around them.
//
// Policy (docs/VM-CONSOLE-DESIGN.md "Privilege model"): every mutation shows
// the EXACT command it runs and everything executed lands in an audit log.
// Verbs delegate to virsh/zfs rather than reimplementing them — the console
// teaches the CLI, never hides it.
//
// What is deliberately NOT here is a confirmation step. Force off, delete
// and rollback used to demand the domain name be retyped; the operator's
// call (2026-08-09) is that these are cattle — a VM is remade from a golden
// in seconds, and the gate taxed every real deletion to insure against a
// rare mistaken one. The audit log is the record that replaced it.
//
// Inputs:  a Row (the selected estate line) + operator-typed parameters.
// Outputs: verbPlan — the command list plus its warning — reported by the
//
//	console as it fires, then executed by runPlan.
//
// Notes: command construction is pure (verbs_test.go); execution is the only
// I/O here. `virsh destroy` on a TRANSIENT domain loses the domain (webui
// lesson) — planForceOff refuses unless the domain is persistent, and
// planDelete skips the undefine that would then fail. Rollback refuses while
// the domain runs, and uses `zfs rollback -r`, which destroys every newer
// snapshot — the warning says so with the count.
//
// WARN: `zfs destroy -r` takes the dataset's snapshots with it. Sanoid
// history on a destroyed zvol is gone with the zvol; only replication to
// another pool or host is a recovery path.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// verbPlan is a mutation ready to run: the argv list(s) to run in order,
// plus what to tell the operator while it happens.
//
// There is no retype gate any more, on any verb. It used to guard force
// off, delete and rollback; the operator's call is that VMs here are cattle
// and typing a hostname to prove you meant the button you pressed is a tax
// on every real deletion to insure against a rare mistaken one. The record
// lives in the audit log instead.
type verbPlan struct {
	title     string     // one-line description, shown as it runs
	cmds      [][]string // executed in order, stop on first failure
	warn      string     // the consequence, surfaced in the status line / TUI
	needsRoot bool       // true for zfs mutations (virsh works via group)

	// post runs only after every cmd succeeded, and CANNOT fail the plan.
	// It is for bookkeeping — registering the result in the inventory — which
	// must never be able to break the operation it is recording. A host with
	// no kldload-db, or a database that will not open, still gets its clone.
	// Failures are written to the audit log and otherwise ignored.
	post [][]string
	// tolerateGone: for a DELETE plan, a virsh destroy/undefine that reports
	// the domain already absent is SUCCESS, because absence is the goal. Only
	// delete sets it — a standalone undefine on a missing domain is still a
	// surprise worth aborting on (see TestAlreadyInDesiredState). This lives on
	// the plan, not in the shared helper, so the two intents stay separate.
	tolerateGone bool
	// undo, when set, is parallel to cmds: undo[i] reverses cmds[i], or is
	// nil when that step leaves nothing behind. When cmds[k] fails, runPlan
	// runs undo[k-1]…undo[0] (newest first) so the plan leaves the host as
	// it found it. Best effort and audited, like post.
	//
	// WHY: a clone is [zfs clone, (zfs clone), virt-clone]. When virt-clone
	// refused, the zvol clones it was meant to attach stayed on the pool
	// with no domain — four of them in one minute on onyx 2026-09-04, each
	// a fresh "zvol without a domain" row for the operator to clean up by
	// hand. The failure was real; the litter was not part of it.
	undo [][]string
}

// virsh() and zfsArgv() live in remote.go — they route to the connection
// target (local or a remote host over ssh).

// cmdLines renders the exact commands, one per line — the contract that the
// operator sees precisely what will run.
func (p verbPlan) cmdLines() string {
	var b strings.Builder
	for _, c := range p.cmds {
		b.WriteString("  $ " + strings.Join(c, " ") + "\n")
	}
	return b.String()
}

// ─── plan builders (pure — tested) ──────────────────────────────────────────

func planStart(r Row) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if r.D.State == "running" {
		return verbPlan{}, fmt.Errorf("%s is already running", r.D.Name)
	}
	return verbPlan{
		title: "start " + r.D.Name,
		cmds:  [][]string{virsh("start", r.D.Name)},
	}, nil
}

func planShutdown(r Row) (verbPlan, error) {
	if r.Synthetic || r.D.State != "running" {
		return verbPlan{}, fmt.Errorf("%s is not running", r.D.Name)
	}
	return verbPlan{
		title: "shut down " + r.D.Name + " (graceful, ACPI)",
		cmds:  [][]string{virsh("shutdown", r.D.Name)},
	}, nil
}

// planForceOff is `virsh destroy` — the plug-pull. Refused for transient
// domains, where destroy doesn't stop the VM, it ERASES it.
func planForceOff(r Row) (verbPlan, error) {
	if r.Synthetic || r.D.State == "shut off" {
		return verbPlan{}, fmt.Errorf("%s is not up", r.D.Name)
	}
	if !r.D.Persistent {
		return verbPlan{}, fmt.Errorf(
			"%s is transient — destroy would erase the domain, not stop it", r.D.Name)
	}
	return verbPlan{
		title: "force off " + r.D.Name,
		cmds:  [][]string{virsh("destroy", r.D.Name)},
		warn:  "pulls the plug — the guest gets no chance to flush",
	}, nil
}

// planSnapshot is a ZFS snapshot of the backing zvol — NOT virsh
// snapshot-create (the webui shipped that wrong concept once). The manual-
// prefix is what the kldload ruleset classes as operator-made.
func planSnapshot(r Row, suffix string) (verbPlan, error) {
	if r.DS == nil {
		return verbPlan{}, fmt.Errorf("no local dataset behind %s", r.D.Name)
	}
	if suffix == "" {
		suffix = time.Now().Format("20060102-150405")
	}
	name := "manual-" + suffix
	// validZFSName, not a blacklist: this suffix ends up inside a zfs argv that
	// the remote login shell re-parses on an ssh target, so `manual-x;reboot`
	// was a command on the hypervisor while passing the old " @/" check.
	if err := validZFSName(suffix); err != nil {
		return verbPlan{}, fmt.Errorf("snapshot %w", err)
	}
	warn := ""
	if r.D.State == "running" {
		warn = "guest is running — snapshot is crash-consistent"
		if r.D.AgentUp {
			warn = "guest is running — snapshot is crash-consistent (0.3 adds agent freeze)"
		}
	}
	return verbPlan{
		title:     "snapshot " + r.DS.Name + "@" + name,
		cmds:      [][]string{zfsArgv("snapshot", r.DS.Name+"@"+name)},
		warn:      warn,
		needsRoot: true,
	}, nil
}

// planReboot / planSuspend / planResume — the light lifecycle verbs every
// right-click menu is expected to carry.
func planReboot(r Row) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if r.D.State != "running" {
		return verbPlan{}, fmt.Errorf("%s is not running", r.D.Name)
	}
	return verbPlan{
		title: "reboot " + r.D.Name,
		cmds:  [][]string{virsh("reboot", r.D.Name)},
	}, nil
}

func planSuspend(r Row) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if r.D.State != "running" {
		return verbPlan{}, fmt.Errorf("%s is not running", r.D.Name)
	}
	return verbPlan{
		title: "suspend " + r.D.Name,
		cmds:  [][]string{virsh("suspend", r.D.Name)},
	}, nil
}

func planResume(r Row) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if r.D.State != "paused" {
		return verbPlan{}, fmt.Errorf("%s is not paused", r.D.Name)
	}
	return verbPlan{
		title: "resume " + r.D.Name,
		cmds:  [][]string{virsh("resume", r.D.Name)},
	}, nil
}

// planDelete removes a VM for good: pull the plug if it is still up,
// undefine (nvram included) and, when a zvol sits underneath, destroy the
// dataset with every snapshot on it. This is the one verb with no undo.
//
// WHY the force-off is part of the plan rather than a precondition: delete
// means delete. Refusing a running domain and telling the operator to go
// force it off first is a two-step dance whose only product is the same
// erased VM, and there is nothing in a guest being destroyed in the next
// command that a graceful shutdown would preserve. Clicking delete is the
// confirmation; there is no second one.
func planDelete(r Row) (verbPlan, error) {
	if r.Synthetic {
		return planReconcile(r)
	}
	var cmds [][]string
	forced := ""
	if r.D.State != "shut off" {
		cmds = append(cmds, virsh("destroy", r.D.Name))
		forced = "forces it off (the guest gets no chance to flush), then "
	}
	// A transient domain has no definition on disk: `virsh destroy` above
	// already erased it, and undefine would then fail and abort the plan
	// before the zvol is destroyed. See planForceOff for the same hazard.
	if r.D.Persistent {
		cmds = append(cmds, virsh("undefine", r.D.Name, "--nvram"))
	}
	warn := forced + "removes the domain definition permanently"
	needsRoot := false
	if r.DS != nil {
		cmds = append(cmds, zfsArgv("destroy", "-r", r.DS.Name))
		warn = forced + "removes the domain AND destroys " + r.DS.Name +
			" with every snapshot and clone under it"
		needsRoot = true
		// The APPLIANCE data disk. Builds attach a second zvol named
		// <root>-data (its in-guest pool), and the estate Row only ever
		// tracked the root — so every deleted appliance orphaned its data
		// disk, snapshots and all. Found on onyx 2026-09-04: `jelly` gone,
		// `jelly-data` still on the pool with a full sanoid snapshot set.
		// Added only when it actually exists, because `zfs destroy` of a
		// missing dataset is deliberately un-forgiven above.
		if datasetExists(r.DS.Name + "-data") {
			cmds = append(cmds, zfsArgv("destroy", "-r", r.DS.Name+"-data"))
			warn += " (and its -data disk)"
		}
	}
	return verbPlan{
		title:     "DELETE " + r.D.Name,
		cmds:      cmds,
		warn:      warn,
		needsRoot: needsRoot,
		// A batch delete often meets a VM that was already half-removed; the
		// domain being gone is exactly what delete wanted, so do not abort
		// before the -data cleanup and the inventory unregister run.
		tolerateGone: true,
		// Drop it from the inventory too. Without this the estate view keeps
		// showing VMs that no longer exist, which is the failure mode that
		// makes people stop trusting an inventory at all.
		//
		// And off the management mesh. The self-test teardown always did
		// this; the verb never did, so a mass delete of test appliances on
		// onyx (2026-09-03) left nine ap-* meshes whose only peer no longer
		// existed, and wgx showed every one of them as an inactive member.
		post: append(append(dbUnregisterVM(r.D.Name), meshTeardownPost(r.D.Name)...),
			seedCleanupPost(r.D.Name)...),
	}, nil
}

// planReconcile is delete for a row with no domain behind it — the
// "unreconciled" group: a register that still claims a VM libvirt no longer
// has, or a zvol nobody references. Delete is the verb the operator reaches
// for on those rows ("shouldn't they be deleted?", onyx 2026-09-03, looking
// at nine yellow ghosts this tool's own delete had left), so it does the
// cleanup the original delete should have: destroy the orphaned storage if
// there is any, forget the register row, take the mesh down. A kspawn
// manifest ghost has no register to forget; the plan still runs and the
// note on the row says where it came from.
func planReconcile(r Row) (verbPlan, error) {
	var cmds [][]string
	warn := "forgets " + r.D.Name + " in the register — nothing in libvirt to remove"
	needsRoot := false
	if r.DS != nil {
		cmds = append(cmds, zfsArgv("destroy", "-r", r.DS.Name))
		warn = "destroys orphaned " + r.DS.Name + " with every snapshot under it"
		needsRoot = true
	}
	return verbPlan{
		title:     "DELETE " + r.D.Name,
		cmds:      cmds,
		warn:      warn,
		needsRoot: needsRoot,
		post: append(append(dbUnregisterVM(r.D.Name), meshTeardownPost(r.D.Name)...),
			seedCleanupPost(r.D.Name)...),
	}, nil
}

// seedCleanupPost removes the VM's NoCloud seed ISO, when one is still on
// the host. The seed carries the guest's user-data — its login hash and
// every recipe secret — and delete left it behind for every VM ever
// removed: 71 of them under /var/lib/libvirt/images on onyx, 2026-09-04,
// for machines that no longer existed. Best effort like the rest of post
// (a missing file is the common case), root because the build installs
// it mode 0600 as root.
func seedCleanupPost(name string) [][]string {
	root := os.Geteuid() == 0
	return [][]string{
		asRoot(root, "rm", "-f", "/var/lib/libvirt/images/"+name+"-seed.iso"),
	}
}

// domainZvols returns the datasets behind every zvol-backed disk of r's
// domain, system disk first, then the rest in the order the domain lists
// them. A row whose Dom carries no disk list (a bare fixture, a remote
// probe that skipped the XML) yields just the system disk.
//
// This is the one place the "which disks does this VM have" question is
// answered for clone and golden. It used to be a naming convention —
// "<root>-data exists on the pool" — which is not the same question: the
// domain XML is what virt-clone walks, and a disk the XML names that the
// pool no longer has is exactly the case the convention cannot see.
func domainZvols(r Row) []string {
	if r.DS == nil {
		return nil
	}
	out := []string{r.DS.Name}
	for _, d := range r.D.Disks {
		ds := zvolDataset(d.Dev)
		if ds == "" || ds == r.DS.Name {
			continue
		}
		out = append(out, ds)
	}
	return out
}

// cloneDatasetName maps a source disk's dataset onto the clone's name:
// <root>-data becomes <new>-data, and a disk with an unrelated name gets
// <new>-<its basename>. Keeping the -data suffix is what lets planDelete
// find the clone's data disk by the same convention the builders use.
func cloneDatasetName(rootDS, newDS, ds string) string {
	if strings.HasPrefix(ds, rootDS+"-") {
		return newDS + strings.TrimPrefix(ds, rootDS)
	}
	return newDS + "-" + baseName(ds)
}

// planClone builds "clone r into newName": snapshot the source zvol, clone
// the snapshot, then virt-clone the domain onto the new zvol with
// --preserve-data (new uuid + MACs, same disk bytes). Generic — works on
// any libvirt host whose guest sits on ZFS and has virt-clone installed;
// the kldload golden-image workflow is exactly this shape.
func planClone(r Row, newName string) (verbPlan, error) {
	return planCloneFrom(r, newName, false)
}

// planCloneFrom is planClone with the choice of anchor: fromGolden clones
// each disk from its @golden snapshot when one exists (no fresh snapshot,
// the clone shares the sealed template's blocks) and falls back to a live
// snapshot+clone per disk that was never sealed — so a golden made before
// data disks were part of one still clones whole.
func planCloneFrom(r Row, newName string, fromGolden bool) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if r.DS == nil {
		return verbPlan{}, fmt.Errorf("no local dataset behind %s — clone needs a zvol", r.D.Name)
	}
	if _, err := exec.LookPath("virt-clone"); err != nil {
		return verbPlan{}, fmt.Errorf("virt-clone not found — install virt-install")
	}
	newName = strings.TrimSpace(newName)
	// The name becomes a dataset component, a snapshot suffix AND a libvirt
	// domain name, so it passes the same allowlist the snapshot suffix does —
	// see validZFSName for why an allowlist and not a metacharacter check.
	if err := validZFSName(newName); err != nil {
		return verbPlan{}, fmt.Errorf("clone %w", err)
	}
	if newName == r.D.Name {
		return verbPlan{}, fmt.Errorf("clone name must differ from the source")
	}
	parent := r.DS.Name
	if i := strings.LastIndexByte(parent, '/'); i >= 0 {
		parent = parent[:i]
	}
	newDS := parent + "/" + newName

	// Every zvol the domain names must still be on the pool. virt-clone
	// walks the XML and refuses a disk whose source is gone ("missing source
	// information for device vdb") — but only after the zfs clones have
	// already been made. Refusing here, before anything is created, turns
	// four orphaned zvols and a one-line dialog into a message that names
	// the disk and what to do about it (onyx, 2026-09-04).
	disks := domainZvols(r)
	for _, d := range r.D.Disks {
		ds := zvolDataset(d.Dev)
		if ds == "" || datasetExists(ds) {
			continue
		}
		return verbPlan{}, fmt.Errorf("%s: disk %s points at %s, which is no longer on the pool"+
			" — recreate it or detach the disk (virsh detach-disk %s %s --persistent) before cloning",
			r.D.Name, d.Target, ds, r.D.Name, d.Target)
	}

	// The system disk must actually have the anchor a golden clone is
	// named for; the GUI offers this verb only when it does, and a missing
	// @golden here means the golden was never made (or was re-made and
	// failed). Extra disks may fall back to a live clone — see the loop.
	if fromGolden && !datasetExists(r.DS.Name+"@golden") {
		return verbPlan{}, fmt.Errorf("%s has no @golden snapshot — make it golden first", r.D.Name)
	}

	var cmds, undo [][]string
	var files []string
	sealed, live := 0, 0
	for _, ds := range disks {
		target := newDS
		if ds != r.DS.Name {
			target = cloneDatasetName(r.DS.Name, newDS, ds)
		}
		if fromGolden && datasetExists(ds+"@golden") {
			cmds = append(cmds, zfsArgv("clone", ds+"@golden", target))
			// -r on the clone target only: it is ours, seconds old, and
			// sanoid may already have snapshotted it — the four orphans of
			// 2026-09-04 each carried four autosnaps within the hour, and a
			// plain destroy of any of them refused "volume has children".
			undo = append(undo, zfsArgv("destroy", "-r", target))
			sealed++
		} else {
			snap := ds + "@clone-" + newName
			cmds = append(cmds, zfsArgv("snapshot", snap), zfsArgv("clone", snap, target))
			undo = append(undo, zfsArgv("destroy", snap), zfsArgv("destroy", "-r", target))
			live++
		}
		// virt-clone --preserve-data takes one --file per disk, in the
		// original's disk order. Without one per zvol a clone of an
		// appliance came up with a root disk and a hole where its pool was.
		files = append(files, "--file", "/dev/zvol/"+target)
	}
	cmds = append(cmds, append([]string{"virt-clone", "--connect", target.LibvirtURI,
		"--original", r.D.Name, "--name", newName, "--preserve-data"}, files...))
	undo = append(undo, nil) // a refused virt-clone defines nothing

	title := "clone " + r.D.Name + " → " + newName
	warn := "clone shares blocks with " + r.DS.Name + " until it diverges"
	if r.D.State == "running" {
		warn = "source is running — the clone is crash-consistent; " + warn
	}
	if fromGolden {
		title = "clone golden " + r.D.Name + " → " + newName
		warn = "clone of the sealed @golden — boots as a fresh machine"
		if live > 0 && sealed > 0 {
			warn += fmt.Sprintf(" (%d disk(s) were never sealed and are cloned live)", live)
		}
	}
	if len(disks) > 1 {
		warn += fmt.Sprintf("; its %d extra disk(s) are cloned with it", len(disks)-1)
	}
	return verbPlan{
		title:     title,
		cmds:      cmds,
		undo:      undo,
		warn:      warn,
		needsRoot: true, // zfs snapshot/clone; virt-clone rides along as root
		// Register the clone in the shared inventory the rest of kldload
		// already writes to — kvm-clone, kvm-create, kube-cluster and klab all
		// do this, and vmxplore was the one path that created VMs without
		// telling anybody. The consequence was visible: three clones running
		// on 2026-08-18 appeared in neither the estate view nor the WireGuard
		// GUI, because both derive membership from that inventory and the
		// DHCP leases, and the clones were in neither.
		//
		// Same argv shape as kvm-clone, so the two paths produce identical
		// rows rather than two dialects of the same fact.
		post: dbRegisterClone(newName, r.D.Name),
	}, nil
}

// asRoot prefixes argv with `sudo -n` unless the caller already is root. It
// is the one spelling of privilege for every host-side bookkeeping command,
// so the audit log shows exactly what ran and a password-prompting host
// fails loudly instead of hanging a hidden prompt.
//
// WHY the inventory writes need it at all: state.db is root's. Called bare,
// `kldload-db vm-delete` failed "attempt to write a readonly database" with
// rc=1 into the audit log and nowhere else, so every VM deleted from this UI
// left libvirt and ZFS and stayed in the inventory — fiend 2026-08-31 (two
// control planes still Ansible targets), onyx 2026-09-03 (all nine deleted
// appliances still rows). The zfs step in the same plan already ran under
// sudo -n; the bookkeeping after it did not.
func asRoot(root bool, argv ...string) []string {
	if root {
		return argv
	}
	return append([]string{"sudo", "-n"}, argv...)
}

// dbRegisterClone returns the inventory calls for a freshly cloned domain, or
// nil when kldload-db is not installed.
//
// Returns nil rather than commands-that-will-fail so the audit log records
// what actually ran. A host without the database is a legitimate
// configuration — vmxplore talks to remote libvirt targets that may not be
// kldload machines at all.
func dbRegisterClone(name, src string) [][]string {
	db := kldloadDB()
	if db == "" {
		return nil
	}
	root := os.Geteuid() == 0
	return [][]string{
		asRoot(root, db, "vm-register", "--name", name, "--role", "clone",
			"--golden-src", src, "--status", "cloned"),
		asRoot(root, db, "event", "--type", "vm", "--subject", name,
			"--message", "cloned from "+src),
	}
}

// dbUnregisterVM returns the inventory calls for a domain being destroyed, or
// nil when kldload-db is not installed. Without this the inventory grows
// monotonically and every deleted VM stays in the estate view forever.
func dbUnregisterVM(name string) [][]string {
	db := kldloadDB()
	if db == "" {
		return nil
	}
	root := os.Geteuid() == 0
	return [][]string{
		asRoot(root, db, "vm-delete", "--name", name),
		asRoot(root, db, "event", "--type", "vm", "--subject", name,
			"--message", "destroyed from vmxplore"),
	}
}

// planRollback rewinds the zvol to a snapshot. -r destroys every snapshot
// newer than the target — the single most dangerous verb here, so it counts
// the casualties into the warning it reports.
func planRollback(r Row, snap string, newer int) (verbPlan, error) {
	if r.DS == nil {
		return verbPlan{}, fmt.Errorf("no local dataset behind this row")
	}
	if !r.Synthetic && r.D.State != "shut off" {
		return verbPlan{}, fmt.Errorf("%s must be shut off to roll back its disk", r.D.Name)
	}
	snapPath := r.DS.Name + "@" + snap
	return verbPlan{
		title:     "rollback " + snapPath,
		cmds:      [][]string{zfsArgv("rollback", "-r", snapPath)},
		warn:      fmt.Sprintf("destroys the %d snapshot(s) newer than @%s", newer, snap),
		needsRoot: true,
	}, nil
}

// planSpecs sets vCPU count and memory via --config: the change lands in the
// persistent definition and takes effect on the next start — safe for both
// running and stopped domains, and the only variant that survives a reboot.
func planSpecs(r Row, vcpus int, memGiB int) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if vcpus < 1 || vcpus > 512 || memGiB < 1 || memGiB > 4096 {
		return verbPlan{}, fmt.Errorf("out of range: %d vcpus / %d GiB", vcpus, memGiB)
	}
	mem := fmt.Sprintf("%dG", memGiB)
	p := verbPlan{
		title: fmt.Sprintf("set %s to %d vcpu / %s (applies on next start)",
			r.D.Name, vcpus, mem),
		cmds: [][]string{
			virsh("setvcpus", r.D.Name, fmt.Sprint(vcpus), "--config", "--maximum"),
			virsh("setvcpus", r.D.Name, fmt.Sprint(vcpus), "--config"),
			virsh("setmaxmem", r.D.Name, mem, "--config"),
			virsh("setmem", r.D.Name, mem, "--config"),
		},
	}
	if r.D.State == "running" {
		p.warn = "domain is running — new specs apply after it stops and starts"
	}
	return p, nil
}

func planAutostart(r Row) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if r.D.Autostart {
		return verbPlan{
			title: "disable autostart for " + r.D.Name,
			cmds:  [][]string{virsh("autostart", "--disable", r.D.Name)},
		}, nil
	}
	return verbPlan{
		title: "enable autostart for " + r.D.Name + " (boots with the host)",
		cmds:  [][]string{virsh("autostart", r.D.Name)},
	}, nil
}

// ─── execution & audit ──────────────────────────────────────────────────────

// alreadyInDesiredState reports whether a failed command failed only because
// the domain was ALREADY in the state that command was trying to reach.
//
// Args:    argv — the command that failed; msg — its combined output.
// Returns: true when the failure is semantically a success.
//
// WHY this exists: runPlan stops at the first failure, which is right for
// almost everything and wrong for `virsh destroy`. A delete plan is
// destroy-then-undefine; a force-off plan is destroy alone. If the domain is
// already off, destroy exits non-zero with "domain is not running" and the
// plan aborts BEFORE undefine — leaving the machine powered off but still
// defined, and every retry failing at the same first step.
//
// HISTORY: 2026-08-12. A delete was issued against a row the estate had
// cached as running while the domain was in fact already stopped. The plan
// force-stopped nothing, failed, and never reached undefine. The operator
// retried and got the identical error, because the stale row kept producing
// the same first command. Cost: a production VM powered off but not removed,
// and an hour spent looking for a build failure that had not happened.
//
// This is deliberately NARROW. Only destroy is forgiven, and only for this one
// message. undefine failing "not found" is NOT forgiven — that would hide a
// plan operating on the wrong domain, which is the opposite of this bug.
func alreadyInDesiredState(argv []string, msg string) bool {
	if len(argv) < 2 {
		return false
	}
	isDestroy := false
	for _, a := range argv[1:] {
		if a == "destroy" {
			isDestroy = true
			break
		}
	}
	// `zfs destroy` shares the word and must never be forgiven: a dataset
	// that is "not found" means the plan is pointed somewhere unexpected.
	if !isDestroy || argv[0] == "zfs" {
		return false
	}
	return strings.Contains(strings.ToLower(msg), "domain is not running")
}

// domainGone recognises libvirt's spellings of "no such domain". Only a plan
// that set tolerateGone consults it, and never for a zfs argv — see the
// field's comment for why the two intents are kept apart.
func domainGone(msg string) bool {
	lm := strings.ToLower(msg)
	return strings.Contains(lm, "failed to get domain") ||
		strings.Contains(lm, "domain not found") ||
		strings.Contains(lm, "no domain with matching")
}

// runPlan executes the plan's commands in order, stopping at the first
// failure. zfs mutations need root; when we aren't root, sudo -n is tried so
// a NOPASSWD host works and a password host fails loudly instead of hanging
// a TUI on a hidden prompt. Every command lands in the audit log either way.
func runPlan(p verbPlan) error {
	for i, argv := range p.cmds {
		// Only the zfs mutation in a mixed plan needs root. Wrapping the
		// whole plan — as this did — also ran virsh as root, which on a
		// remote target means root's ssh keys and a connection the
		// operator never authorized. ssh-transported zfs carries its own
		// privilege on the far side, so it is left alone too.
		if p.needsRoot && argv[0] == "zfs" && os.Geteuid() != 0 {
			argv = append([]string{"sudo", "-n"}, argv...)
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		rc := 0
		if err != nil {
			rc = 1
		}
		auditLog(strings.Join(argv, " "), rc)
		// A zvol stays "busy" for a moment after the domain that had it
		// open is destroyed: qemu releases the block device asynchronously,
		// and `zfs destroy` run in the same second as `virsh destroy` fails
		// "dataset is busy". The plan then stopped with the domain gone and
		// the disk, the inventory row and the mesh all still there — the
		// operator saw a dialog and a VM that was half deleted (onyx,
		// 2026-09-04 16:36, `test`; the same at 08:15 and on 2026-08-31).
		// Transient by nature and bounded here: the wait is a few seconds
		// at most, each attempt is audited, and a dataset that is busy for
		// a real reason (still attached elsewhere) fails after the last try
		// with the same message it always had.
		for attempt := 1; err != nil && attempt <= busyDestroyRetries &&
			busyDestroy(argv, string(out)); attempt++ {
			time.Sleep(busyDestroyDelay(attempt))
			out, err = exec.Command(argv[0], argv[1:]...).CombinedOutput()
			rc = 0
			if err != nil {
				rc = 1
			}
			auditLog(fmt.Sprintf("%s (retry %d, dataset was busy)", strings.Join(argv, " "), attempt), rc)
		}
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			if alreadyInDesiredState(argv, msg) {
				// Not a failure: the step asked for a state the domain is
				// already in. Continuing is the whole point — see the
				// function's comment for what aborting here cost.
				continue
			}
			if p.tolerateGone && argv[0] != "zfs" && domainGone(msg) {
				// Delete's target is already gone — keep going so the zfs
				// cleanup, the mesh teardown and the inventory unregister
				// still run.
				continue
			}
			return fmt.Errorf("%s: %s%s", argv[0], msg, unwind(p, i))
		}
	}
	// Bookkeeping, best effort. Deliberately after the success return path of
	// every cmd, and deliberately incapable of returning an error: an estate
	// that refuses to clone because it could not write an inventory row is
	// worse than one whose inventory is briefly behind.
	for _, argv := range p.post {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		rc := 0
		if err != nil {
			rc = 1
		}
		auditLog(strings.Join(argv, " "), rc)
		_ = out
	}
	return nil
}

// unwind runs the undo steps for every cmd before the one that failed at
// index failed, newest first, and returns a suffix for the error message
// saying what was put back — or which undo itself failed, since a rollback
// that half-worked is something the operator has to know about.
//
// Root is added exactly as runPlan adds it for the forward step, so an undo
// never runs with more privilege than the thing it reverses.
func unwind(p verbPlan, failed int) string {
	if len(p.undo) == 0 {
		return ""
	}
	var done, broken []string
	for j := failed - 1; j >= 0; j-- {
		if j >= len(p.undo) || p.undo[j] == nil {
			continue
		}
		argv := p.undo[j]
		if p.needsRoot && argv[0] == "zfs" && os.Geteuid() != 0 {
			argv = append([]string{"sudo", "-n"}, argv...)
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		rc := 0
		if err != nil {
			rc = 1
		}
		auditLog(strings.Join(argv, " "), rc)
		line := strings.Join(p.undo[j], " ")
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			broken = append(broken, line+" ("+msg+")")
			continue
		}
		done = append(done, line)
	}
	s := ""
	if len(done) > 0 {
		s += "\nrolled back: " + strings.Join(done, "; ")
	}
	if len(broken) > 0 {
		s += "\nROLLBACK FAILED, clean up by hand: " + strings.Join(broken, "; ")
	}
	return s
}

// busyDestroyRetries bounds the wait for a released zvol: the delays below
// sum to about 12 s, longer than qemu has ever taken to let go of a disk
// here and short enough that a genuinely held dataset still fails while
// the operator is watching.
const busyDestroyRetries = 8

// busyDestroyDelay grows with the attempt: the first retry is almost
// immediate, since the usual case is a release that lands milliseconds
// after virsh returns.
func busyDestroyDelay(attempt int) time.Duration {
	d := 250 * time.Millisecond << uint(attempt-1)
	if d > 3*time.Second {
		d = 3 * time.Second
	}
	return d
}

// busyDestroy reports whether argv is a `zfs destroy` (bare or under sudo)
// that failed because the dataset is busy — the one zfs failure worth
// retrying.
func busyDestroy(argv []string, out string) bool {
	if !strings.Contains(strings.ToLower(out), "dataset is busy") {
		return false
	}
	for i, a := range argv {
		if a == "zfs" && i+1 < len(argv) && argv[i+1] == "destroy" {
			return true
		}
	}
	return false
}

// auditLogFailed fires once, ever, when neither audit path can be written.
// Best-effort must not mean silent: "every mutation is audited" is a claim
// this tool makes, so an operator on a host where it is not true deserves to
// know — once, on stderr, without a warning per verb.
var auditLogFailed sync.Once

// auditLog appends "when | who | command | rc" to /var/log/kldload/vmx.log,
// falling back to the user's state dir when that path isn't writable (non-
// root on a non-kldload host). Best-effort by design: an unwritable audit
// trail must not block an already-confirmed verb, and the command itself
// still shows in the status line — but it does say so once, see above.
func auditLog(cmdline string, rc int) {
	who := "?"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	line := fmt.Sprintf("%s | %s | %s | rc=%d\n",
		time.Now().Format(time.RFC3339), who, cmdline, rc)
	for _, path := range []string{
		"/var/log/kldload/vmx.log",
		filepath.Join(os.Getenv("HOME"), ".local/state/vmxplore/vmx.log"),
	} {
		// MkdirAll only for the fallback; /var/log/kldload exists on kldload
		if strings.HasPrefix(path, os.Getenv("HOME")) && os.Getenv("HOME") != "" {
			// error ignored: fallback dir creation failing means the write
			// below fails too, and we are out of places to log
			_ = os.MkdirAll(filepath.Dir(path), 0o700)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			continue
		}
		_, werr := f.WriteString(line)
		// error ignored on close: the write above already succeeded or not
		_ = f.Close()
		if werr == nil {
			return
		}
	}
	auditLogFailed.Do(func() {
		fmt.Fprintln(os.Stderr,
			"vmx: WARNING: audit log not writable (/var/log/kldload/vmx.log or "+
				"~/.local/state/vmxplore/vmx.log) — commands still run and are "+
				"shown, but nothing is being recorded")
	})
}
