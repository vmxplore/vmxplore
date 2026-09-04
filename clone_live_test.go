// clone_live_test.go — a golden clone, end to end, against the real host.
// Gated behind VMX_CLONE_E2E=1: it needs a sealed golden with a data disk
// (VMX_CLONE_SRC, default web-golden), uses sudo -n for zfs, defines a
// scratch domain and removes it again.
//
// WHY: the clone plan is pure and tested, but what broke on onyx on
// 2026-09-04 was the seam between the plan and virt-clone — a disk the
// domain named that the pool no longer had. Only a real run through
// virt-clone proves the two agree on the disk list.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestGoldenCloneE2E(t *testing.T) {
	if os.Getenv("VMX_CLONE_E2E") != "1" {
		t.Skip("set VMX_CLONE_E2E=1 to clone a real golden")
	}
	src := os.Getenv("VMX_CLONE_SRC")
	if src == "" {
		src = "web-golden"
	}
	lv, err := ConnectSystem()
	if err != nil {
		t.Skipf("no libvirt: %v", err)
	}
	defer lv.Close()
	doms, err := lv.Estate()
	if err != nil {
		t.Fatal(err)
	}
	dss, err := ListDatasets()
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	var row Row
	found := false
	for _, g := range BuildEstate(doms, dss, snaps, kldloadRS(t), LoadAnnotations()) {
		for _, r := range g.Rows {
			if r.D.Name == src {
				row, found = r, true
			}
		}
	}
	if !found || row.DS == nil {
		t.Fatalf("%s is not a zvol-backed domain on this host", src)
	}
	zvols := domainZvols(row)
	if len(zvols) < 2 {
		t.Fatalf("%s has %d zvol(s); this test wants a golden with a data disk", src, len(zvols))
	}

	const name = "vmx-clone-e2e"
	cleanup := func() {
		exec.Command("virsh", "-c", "qemu:///system", "undefine", name, "--nvram").Run()
		for _, ds := range []string{"rpool/vms/" + name, "rpool/vms/" + name + "-data"} {
			exec.Command("sudo", "-n", "zfs", "destroy", "-r", ds).Run()
		}
	}
	cleanup()
	defer cleanup()

	p, err := planCloneGolden(row, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(p.cmdLines())
	if err := runPlan(p); err != nil {
		t.Fatalf("clone: %v", err)
	}

	// The clone must name BOTH zvols, each a clone of the source's disk.
	out, err := exec.Command("virsh", "-c", "qemu:///system", "domblklist", name).Output()
	if err != nil {
		t.Fatalf("domblklist: %v", err)
	}
	for _, want := range []string{"/dev/zvol/rpool/vms/" + name, "/dev/zvol/rpool/vms/" + name + "-data"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("clone is missing %s:\n%s", want, out)
		}
	}
	for _, ds := range []string{"rpool/vms/" + name, "rpool/vms/" + name + "-data"} {
		o, err := exec.Command("zfs", "list", "-H", "-o", "origin", ds).Output()
		if err != nil || !strings.Contains(string(o), "@golden") {
			t.Errorf("%s origin = %q %v, want a @golden clone", ds, o, err)
		}
	}

	// And delete must take both zvols with the domain.
	doms, _ = lv.Estate()
	dss, _ = ListDatasets()
	var crow Row
	for _, g := range BuildEstate(doms, dss, snaps, kldloadRS(t), LoadAnnotations()) {
		for _, r := range g.Rows {
			if r.D.Name == name {
				crow = r
			}
		}
	}
	dp, err := planDelete(crow)
	if err != nil {
		t.Fatal(err)
	}
	if err := runPlan(dp); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, ds := range []string{"rpool/vms/" + name, "rpool/vms/" + name + "-data"} {
		if datasetExists(ds) {
			t.Errorf("%s survived delete", ds)
		}
	}
}
