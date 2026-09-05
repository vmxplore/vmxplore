// verbs_test.go — the plan builders are the safety layer; test the gates.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runningRow() Row {
	return Row{D: Dom{Name: "klab-blue-fedora", State: "running",
		Persistent: true, VCPUs: 1, MaxMemKiB: 384 * 1024},
		DS: &Dataset{Name: "rpool/vms/klab-blue-fedora", Type: "volume"}}
}

func offRow() Row {
	r := runningRow()
	r.D.State = "shut off"
	return r
}

func TestPlanStartAndShutdownGates(t *testing.T) {
	if _, err := planStart(runningRow()); err == nil {
		t.Error("start of a running domain must refuse")
	}
	p, err := planStart(offRow())
	if err != nil || strings.Join(p.cmds[0], " ") != "virsh -c qemu:///system start klab-blue-fedora" {
		t.Errorf("start plan wrong: %v %v", p.cmds, err)
	}
	if _, err := planShutdown(offRow()); err == nil {
		t.Error("shutdown of a stopped domain must refuse")
	}
}

func TestPlanForceOffGates(t *testing.T) {
	p, err := planForceOff(runningRow())
	if err != nil {
		t.Fatal(err)
	}
	if p.warn == "" {
		t.Error("force-off must warn that the guest gets no chance to flush")
	}
	// the webui lesson: destroy on a transient domain erases it
	tr := runningRow()
	tr.D.Persistent = false
	if _, err := planForceOff(tr); err == nil {
		t.Error("force-off of a transient domain must refuse")
	}
}

func TestPlanSnapshot(t *testing.T) {
	p, err := planSnapshot(offRow(), "before-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	want := "zfs snapshot rpool/vms/klab-blue-fedora@manual-before-upgrade"
	if strings.Join(p.cmds[0], " ") != want {
		t.Errorf("snapshot cmd = %v", p.cmds[0])
	}
	if !p.needsRoot {
		t.Error("zfs snapshot must be marked needsRoot")
	}
	if _, err := planSnapshot(offRow(), "bad name"); err == nil {
		t.Error("snapshot suffix with a space must refuse")
	}
	if p, _ := planSnapshot(runningRow(), "x"); p.warn == "" {
		t.Error("running-domain snapshot must warn crash-consistent")
	}
}

func TestPlanRollbackGates(t *testing.T) {
	if _, err := planRollback(runningRow(), "manual-x", 3); err == nil {
		t.Error("rollback of a running domain must refuse")
	}
	p, err := planRollback(offRow(), "manual-x", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.warn, "3 snapshot") {
		t.Errorf("rollback must count its casualties: warn=%q", p.warn)
	}
	want := "zfs rollback -r rpool/vms/klab-blue-fedora@manual-x"
	if strings.Join(p.cmds[0], " ") != want {
		t.Errorf("rollback cmd = %v", p.cmds[0])
	}
}

func TestPlanSpecs(t *testing.T) {
	p, err := planSpecs(offRow(), 4, 8)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range p.cmds {
		joined += strings.Join(c, " ") + ";"
	}
	for _, want := range []string{
		"virsh -c qemu:///system setvcpus klab-blue-fedora 4 --config --maximum",
		"virsh -c qemu:///system setvcpus klab-blue-fedora 4 --config",
		"virsh -c qemu:///system setmaxmem klab-blue-fedora 8G --config",
		"virsh -c qemu:///system setmem klab-blue-fedora 8G --config",
	} {
		if !strings.Contains(joined, want+";") {
			t.Errorf("specs plan missing %q in %s", want, joined)
		}
	}
	if _, err := planSpecs(offRow(), 0, 8); err == nil {
		t.Error("0 vcpus must refuse")
	}
	if p, _ := planSpecs(runningRow(), 2, 4); p.warn == "" {
		t.Error("running-domain spec change must warn next-start")
	}
}

func TestPlanLightVerbs(t *testing.T) {
	if _, err := planReboot(offRow()); err == nil {
		t.Error("reboot of a stopped domain must refuse")
	}
	if p, err := planReboot(runningRow()); err != nil ||
		strings.Join(p.cmds[0], " ") != "virsh -c qemu:///system reboot klab-blue-fedora" {
		t.Errorf("reboot plan wrong: %v %v", p.cmds, err)
	}
	if _, err := planSuspend(offRow()); err == nil {
		t.Error("suspend of a stopped domain must refuse")
	}
	if _, err := planResume(runningRow()); err == nil {
		t.Error("resume of a running (not paused) domain must refuse")
	}
	paused := runningRow()
	paused.D.State = "paused"
	if _, err := planResume(paused); err != nil {
		t.Errorf("resume of a paused domain must plan: %v", err)
	}
}

func TestPlanDeleteGates(t *testing.T) {
	// Delete means delete: a running domain is forced off inside the same
	// plan rather than refused, and the force-off has to come first.
	run, err := planDelete(runningRow())
	if err != nil {
		t.Fatalf("delete of a running domain must plan, not refuse: %v", err)
	}
	if len(run.cmds) == 0 ||
		strings.Join(run.cmds[0], " ") != "virsh -c qemu:///system destroy klab-blue-fedora" {
		t.Errorf("delete of a running domain must force off first: %v", run.cmds)
	}
	if run.warn == "" || !strings.Contains(run.warn, "forces it off") {
		t.Errorf("delete of a running domain must say it forces off: %q", run.warn)
	}
	// A transient domain is erased by the destroy above; undefine would
	// then fail and abort the plan before the zvol is destroyed.
	tr := runningRow()
	tr.D.Persistent = false
	trJoined := ""
	trPlan, _ := planDelete(tr)
	for _, c := range trPlan.cmds {
		trJoined += strings.Join(c, " ") + ";"
	}
	if strings.Contains(trJoined, "undefine") {
		t.Errorf("transient delete must not undefine: %s", trJoined)
	}

	p, err := planDelete(offRow())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.warn, "every snapshot") {
		t.Errorf("delete must spell out what the zvol takes with it: %q", p.warn)
	}
	joined := ""
	for _, c := range p.cmds {
		joined += strings.Join(c, " ") + ";"
	}
	if !strings.Contains(joined, "undefine klab-blue-fedora --nvram;") ||
		!strings.Contains(joined, "zfs destroy -r rpool/vms/klab-blue-fedora;") {
		t.Errorf("delete plan wrong: %s", joined)
	}
	noDS := offRow()
	noDS.DS = nil
	if p, _ := planDelete(noDS); len(p.cmds) != 1 || p.needsRoot {
		t.Error("delete without a dataset must only undefine, unprivileged")
	}
}

func TestPlanClone(t *testing.T) {
	if _, err := exec.LookPath("virt-clone"); err != nil {
		t.Skip("virt-clone not installed on this host")
	}
	p, err := planClone(offRow(), "klab-copy")
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range p.cmds {
		joined += strings.Join(c, " ") + ";"
	}
	for _, want := range []string{
		"zfs snapshot rpool/vms/klab-blue-fedora@clone-klab-copy",
		"zfs clone rpool/vms/klab-blue-fedora@clone-klab-copy rpool/vms/klab-copy",
		"virt-clone --connect qemu:///system --original klab-blue-fedora" +
			" --name klab-copy --preserve-data --file /dev/zvol/rpool/vms/klab-copy",
	} {
		if !strings.Contains(joined, want+";") {
			t.Errorf("clone plan missing %q in %s", want, joined)
		}
	}
	if !p.needsRoot {
		t.Error("clone must be marked needsRoot (zfs)")
	}
	if _, err := planClone(offRow(), "bad name"); err == nil {
		t.Error("clone name with a space must refuse")
	}
	if _, err := planClone(offRow(), "klab-blue-fedora"); err == nil {
		t.Error("clone onto the source name must refuse")
	}
	noDS := offRow()
	noDS.DS = nil
	if _, err := planClone(noDS, "x"); err == nil {
		t.Error("clone without a dataset must refuse")
	}
}

func TestPlanCloneGolden(t *testing.T) {
	if _, err := exec.LookPath("virt-clone"); err != nil {
		t.Skip("virt-clone not installed on this host")
	}
	stubDatasets(t, "rpool/vms/klab-blue-fedora", "rpool/vms/klab-blue-fedora@golden")
	p, err := planCloneGolden(offRow(), "cloned")
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range p.cmds {
		joined += strings.Join(c, " ") + ";"
	}
	if !strings.Contains(joined,
		"zfs clone rpool/vms/klab-blue-fedora@golden rpool/vms/cloned;") {
		t.Errorf("golden clone must clone @golden, got %s", joined)
	}
	if strings.Contains(joined, "zfs snapshot") {
		t.Error("golden clone must NOT take a fresh snapshot")
	}
	stubDatasets(t, "rpool/vms/klab-blue-fedora") // never made golden
	if _, err := planCloneGolden(offRow(), "cloned"); err == nil {
		t.Error("golden clone of a source with no @golden must refuse")
	}
}

func TestPlanAutostartToggle(t *testing.T) {
	p, _ := planAutostart(offRow())
	if strings.Join(p.cmds[0], " ") != "virsh -c qemu:///system autostart klab-blue-fedora" {
		t.Errorf("autostart enable cmd = %v", p.cmds[0])
	}
	on := offRow()
	on.D.Autostart = true
	p, _ = planAutostart(on)
	if strings.Join(p.cmds[0], " ") != "virsh -c qemu:///system autostart --disable klab-blue-fedora" {
		t.Errorf("autostart disable cmd = %v", p.cmds[0])
	}
}

// A delete plan is destroy-then-undefine. If the domain is already off,
// destroy exits non-zero and — before this — aborted the plan, leaving the
// machine powered off but still defined, with every retry failing identically.
//
// HISTORY: 2026-08-12, hit on a production VM. The estate had cached the row
// as running while the domain was already stopped, so the plan kept emitting
// the same doomed first command.
func TestAlreadyInDesiredState(t *testing.T) {
	const notRunning = "error: Failed to destroy domain 'blog'\n" +
		"error: Requested operation is not valid: domain is not running"

	for _, tc := range []struct {
		name string
		argv []string
		msg  string
		want bool
	}{
		{"destroy on a stopped domain is not a failure",
			[]string{"virsh", "destroy", "blog"}, notRunning, true},
		{"destroy through a remote target still forgiven",
			[]string{"virsh", "-c", "qemu+ssh://h/system", "destroy", "blog"},
			notRunning, true},

		// Everything below must NOT be forgiven.
		{"destroy failing for any other reason",
			[]string{"virsh", "destroy", "blog"},
			"error: Failed to destroy domain 'blog': permission denied", false},
		{"undefine on a missing domain must still abort",
			[]string{"virsh", "undefine", "blog"},
			"error: failed to get domain 'blog'", false},
		{"zfs destroy is never forgiven — wrong dataset is the nightmare",
			[]string{"zfs", "destroy", "-r", "rpool/vms/blog"},
			"cannot open 'rpool/vms/blog': domain is not running", false},
		{"start failing is a real failure",
			[]string{"virsh", "start", "blog"},
			"error: domain is not running", false},
		{"a malformed argv is not forgiven",
			[]string{"virsh"}, notRunning, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := alreadyInDesiredState(tc.argv, tc.msg); got != tc.want {
				t.Errorf("alreadyInDesiredState(%v) = %v, want %v",
					tc.argv, got, tc.want)
			}
		})
	}
}

// TestDeleteTolerance pins two failures from onyx, 2026-09-03: appliance
// deletes orphaned the -data companion zvol and the ap-* mesh, and a batch
// delete that met an already-gone domain aborted before its post steps ran.
func TestDeleteTolerance(t *testing.T) {
	p, err := planDelete(offRow())
	if err != nil {
		t.Fatal(err)
	}
	if !p.tolerateGone {
		t.Error("delete must tolerate an already-absent domain")
	}
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"error: failed to get domain 'blog'", true},
		{"error: Domain not found: no domain with matching name 'blog'", true},
		{"error: Failed to destroy domain 'blog': permission denied", false},
		{"error: Requested operation is not valid: domain is not running", false},
		{"", false},
	} {
		if got := domainGone(tc.msg); got != tc.want {
			t.Errorf("domainGone(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	// The mesh teardown shape: down first, then the network resync, under
	// sudo -n for an unprivileged console and bare for root.
	got := meshTeardownArgv("ap-blog", false, true)
	if len(got) != 2 ||
		strings.Join(got[0], " ") != "sudo -n kvm-mesh down ap-blog" ||
		strings.Join(got[1], " ") != "sudo -n kldload-networks sync" {
		t.Errorf("mesh teardown argv = %v", got)
	}
	if got := meshTeardownArgv("ap-blog", true, false); len(got) != 1 ||
		strings.Join(got[0], " ") != "kvm-mesh down ap-blog" {
		t.Errorf("root mesh teardown argv = %v", got)
	}
	// The inventory write is root's, so it carries sudo -n exactly like the
	// zfs step before it — bare, it failed silently and the row stayed.
	if got := asRoot(false, "kldload-db", "vm-delete", "--name", "blog"); strings.Join(got, " ") !=
		"sudo -n kldload-db vm-delete --name blog" {
		t.Errorf("unprivileged inventory argv = %v", got)
	}
	if got := asRoot(true, "kldload-db", "vm-delete"); strings.Join(got, " ") != "kldload-db vm-delete" {
		t.Errorf("root inventory argv = %v", got)
	}
	// Delete on an unreconciled row reconciles it instead of refusing: a
	// register ghost has nothing to run and only bookkeeping to do; an
	// orphaned zvol is destroyed, and that needs root.
	ghost := Row{D: Dom{Name: "jelly", State: "absent"}, Synthetic: true}
	gp, err := planDelete(ghost)
	if err != nil || len(gp.cmds) != 0 || gp.needsRoot {
		t.Errorf("ghost delete = %+v, %v; want no commands, unprivileged", gp.cmds, err)
	}
	orphan := offRow()
	orphan.Synthetic = true
	orphan.D.State = "no domain"
	op, err := planDelete(orphan)
	if err != nil || len(op.cmds) != 1 || !op.needsRoot ||
		strings.Join(op.cmds[0], " ") != "zfs destroy -r rpool/vms/klab-blue-fedora" {
		t.Errorf("orphan delete = %+v, %v; want one zfs destroy as root", op.cmds, err)
	}
	// The mesh name is the interface name and the kernel caps that at 15.
	if m := enrollMeshName("a-very-long-appliance-name"); m != "ap-a-very-long" {
		t.Errorf("enrollMeshName = %q", m)
	}
}

// stubDatasets answers datasetExists from a fixed set for the duration of
// one test, so clone plans can be exercised for disks and @golden anchors
// the test host does not have.
func stubDatasets(t *testing.T, have ...string) {
	t.Helper()
	orig := datasetExists
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	datasetExists = func(name string) bool { return set[name] }
	t.Cleanup(func() { datasetExists = orig })
}

// applianceRow is offRow with the disk list an appliance build defines:
// root zvol, -data zvol, seed cdrom — the web-golden shape from onyx.
func applianceRow() Row {
	r := offRow()
	r.D.Disks = []Disk{
		{Target: "vda", Dev: "/dev/zvol/rpool/vms/klab-blue-fedora"},
		{Target: "vdb", Dev: "/dev/zvol/rpool/vms/klab-blue-fedora-data"},
		{Target: "sda", File: "/var/lib/libvirt/images/klab-blue-fedora-seed.iso"},
	}
	return r
}

func joinCmds(cmds [][]string) string {
	joined := ""
	for _, c := range cmds {
		joined += strings.Join(c, " ") + ";"
	}
	return joined
}

// Every zvol the domain names is cloned and handed to virt-clone, in disk
// order, and each creating step carries its undo.
func TestPlanCloneCarriesEveryZvol(t *testing.T) {
	if _, err := exec.LookPath("virt-clone"); err != nil {
		t.Skip("virt-clone not installed on this host")
	}
	stubDatasets(t, "rpool/vms/klab-blue-fedora", "rpool/vms/klab-blue-fedora-data")
	p, err := planClone(applianceRow(), "copy")
	if err != nil {
		t.Fatal(err)
	}
	joined := joinCmds(p.cmds)
	for _, want := range []string{
		"zfs snapshot rpool/vms/klab-blue-fedora@clone-copy;",
		"zfs clone rpool/vms/klab-blue-fedora@clone-copy rpool/vms/copy;",
		"zfs snapshot rpool/vms/klab-blue-fedora-data@clone-copy;",
		"zfs clone rpool/vms/klab-blue-fedora-data@clone-copy rpool/vms/copy-data;",
		"--preserve-data --file /dev/zvol/rpool/vms/copy --file /dev/zvol/rpool/vms/copy-data;",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("clone plan missing %q in %s", want, joined)
		}
	}
	if len(p.undo) != len(p.cmds) {
		t.Fatalf("undo must be parallel to cmds: %d vs %d", len(p.undo), len(p.cmds))
	}
	if p.undo[len(p.undo)-1] != nil {
		t.Error("virt-clone step must have no undo — a refused clone defines nothing")
	}
	if got := strings.Join(p.undo[1], " "); got != "zfs destroy -r rpool/vms/copy" {
		t.Errorf("undo for the root clone = %q", got)
	}
}

// A disk the domain still names but the pool no longer has must refuse
// before anything is created — the onyx 2026-09-04 web-golden case.
func TestPlanCloneRefusesMissingDisk(t *testing.T) {
	if _, err := exec.LookPath("virt-clone"); err != nil {
		t.Skip("virt-clone not installed on this host")
	}
	stubDatasets(t, "rpool/vms/klab-blue-fedora") // -data is gone
	p, err := planClone(applianceRow(), "copy")
	if err == nil {
		t.Fatalf("clone with a missing disk must refuse, got %s", joinCmds(p.cmds))
	}
	for _, want := range []string{"vdb", "rpool/vms/klab-blue-fedora-data", "detach"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q: %v", want, err)
		}
	}
	if _, err := planCloneGolden(applianceRow(), "copy"); err == nil {
		t.Error("golden clone with a missing disk must refuse too")
	}
}

// A golden clone anchors each disk on its own @golden when it has one and
// falls back to a live snapshot for a disk that was never sealed.
func TestPlanCloneGoldenPerDiskAnchor(t *testing.T) {
	if _, err := exec.LookPath("virt-clone"); err != nil {
		t.Skip("virt-clone not installed on this host")
	}
	stubDatasets(t, "rpool/vms/klab-blue-fedora", "rpool/vms/klab-blue-fedora-data",
		"rpool/vms/klab-blue-fedora@golden")
	p, err := planCloneGolden(applianceRow(), "cloned")
	if err != nil {
		t.Fatal(err)
	}
	joined := joinCmds(p.cmds)
	if !strings.Contains(joined, "zfs clone rpool/vms/klab-blue-fedora@golden rpool/vms/cloned;") {
		t.Errorf("root must clone @golden: %s", joined)
	}
	if !strings.Contains(joined, "zfs snapshot rpool/vms/klab-blue-fedora-data@clone-cloned;") {
		t.Errorf("unsealed data disk must be cloned live: %s", joined)
	}

	stubDatasets(t, "rpool/vms/klab-blue-fedora", "rpool/vms/klab-blue-fedora-data",
		"rpool/vms/klab-blue-fedora@golden", "rpool/vms/klab-blue-fedora-data@golden")
	p, err = planCloneGolden(applianceRow(), "cloned")
	if err != nil {
		t.Fatal(err)
	}
	joined = joinCmds(p.cmds)
	if strings.Contains(joined, "zfs snapshot") {
		t.Errorf("fully sealed golden must take no snapshot: %s", joined)
	}
	if !strings.Contains(joined, "zfs clone rpool/vms/klab-blue-fedora-data@golden rpool/vms/cloned-data;") {
		t.Errorf("data disk must clone its @golden: %s", joined)
	}
}

func TestCloneDatasetName(t *testing.T) {
	for _, c := range [][3]string{
		{"rpool/vms/web-golden-data", "rpool/vms/x", "rpool/vms/x-data"},
		{"rpool/vms/web-golden-media", "rpool/vms/x", "rpool/vms/x-media"},
		{"rpool/isos/shared", "rpool/vms/x", "rpool/vms/x-shared"},
	} {
		if got := cloneDatasetName("rpool/vms/web-golden", c[1], c[0]); got != c[2] {
			t.Errorf("cloneDatasetName(%s) = %s, want %s", c[0], got, c[2])
		}
	}
}

// When a later step fails, runPlan runs the undo of every earlier step,
// newest first, and says so in the error.
func TestRunPlanUnwindsOnFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep the audit log out of the real one
	dir := t.TempDir()
	a, b := dir+"/a", dir+"/b"
	p := verbPlan{
		cmds: [][]string{{"touch", a}, {"touch", b}, {"false"}},
		undo: [][]string{{"rm", a}, {"rm", b}, nil},
	}
	err := runPlan(p)
	if err == nil {
		t.Fatal("plan with a failing step must error")
	}
	if !strings.Contains(err.Error(), "rolled back: rm "+b+"; rm "+a) {
		t.Errorf("error must list the undo steps newest first: %v", err)
	}
	for _, f := range []string{a, b} {
		if _, statErr := os.Stat(f); statErr == nil {
			t.Errorf("%s still exists after unwind", f)
		}
	}
	// A broken undo is reported, not hidden.
	p = verbPlan{
		cmds: [][]string{{"true"}, {"false"}},
		undo: [][]string{{"rm", dir + "/never-there"}, nil},
	}
	err = runPlan(p)
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK FAILED") {
		t.Errorf("failed undo must be named in the error: %v", err)
	}
}

// A `zfs destroy` that reports "dataset is busy" is retried, and the plan
// goes on to its later steps once the dataset lets go. A fake zfs on PATH
// fails twice and then succeeds.
func TestRunPlanRetriesBusyDestroy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	counter := bin + "/calls"
	script := "#!/bin/sh\n" +
		"n=$(cat " + counter + " 2>/dev/null || echo 0); n=$((n+1)); echo $n >" + counter + "\n" +
		"if [ $n -le 2 ]; then echo \"cannot destroy 'rpool/vms/x': dataset is busy\" >&2; exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin+"/zfs", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	marker := bin + "/after"
	p := verbPlan{cmds: [][]string{
		{"zfs", "destroy", "-r", "rpool/vms/x"},
		{"touch", marker},
	}}
	if err := runPlan(p); err != nil {
		t.Fatalf("busy destroy must be retried, got %v", err)
	}
	if n, _ := os.ReadFile(counter); strings.TrimSpace(string(n)) != "3" {
		t.Errorf("zfs was called %s times, want 3", strings.TrimSpace(string(n)))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the step after the busy destroy did not run")
	}
	// Not every zfs failure is retried: a missing dataset fails at once.
	os.Remove(counter)
	gone := "#!/bin/sh\necho \"cannot open 'rpool/vms/x': dataset does not exist\" >&2; exit 1\n"
	if err := os.WriteFile(bin+"/zfs", []byte(gone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runPlan(p); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing dataset must fail immediately, got %v", err)
	}
	if !busyDestroy([]string{"sudo", "-n", "zfs", "destroy", "x"}, "dataset is busy") {
		t.Error("busyDestroy must see through sudo -n")
	}
	if busyDestroy([]string{"zfs", "snapshot", "x"}, "dataset is busy") {
		t.Error("only destroy is retried")
	}
}

// Delete takes the seed ISO with the VM: it holds the guest's login hash
// and the recipe's secrets, and it used to outlive every VM.
func TestPlanDeleteRemovesSeed(t *testing.T) {
	p, err := planDelete(offRow())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range p.post {
		if strings.HasSuffix(strings.Join(c, " "), "rm -f /var/lib/libvirt/images/klab-blue-fedora-seed.iso") {
			found = true
		}
	}
	if !found {
		t.Errorf("delete post steps do not remove the seed ISO: %v", p.post)
	}
}
