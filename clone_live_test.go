// clone_live_test.go — a golden clone, end to end, against the real host.
// Gated behind VMX_CLONE_E2E=1: it needs a sealed golden with a data disk
// (VMX_CLONE_SRC, default web-golden), uses sudo -n for zfs, defines a
// scratch domain, boots it, checks its pool imported (and its service
// answers on VMX_CLONE_PORT when set), and removes it again.
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
	"time"
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
		exec.Command("virsh", "-c", "qemu:///system", "destroy", name).Run()
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

	// Start it, then delete it RUNNING: that is the GUI's common case, and
	// the one where `zfs destroy` lands in the same second as `virsh
	// destroy` and meets "dataset is busy" while qemu lets go of the zvol.
	// runPlan retries that; before it did, the domain was gone and the
	// disk, inventory row and mesh all stayed (onyx, 2026-09-04 16:36).
	if out, err := exec.Command("virsh", "-c", "qemu:///system", "start", name).CombinedOutput(); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	time.Sleep(8 * time.Second) // let qemu open the disks for real

	// A clone of an appliance golden has to come up WITH its pool and its
	// service, not just boot. Five clones of web-golden came up on
	// 2026-09-04 with nginx answering 404: the pool was never imported on
	// the clone, so everything the recipe put on tank/www was missing. The
	// clone's reseeded first boot does not re-run the recipe; the data disk
	// has to arrive whole from the golden and import on its own.
	var ip string
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); {
		if ips, err := lv.LeaseIPs(name); err == nil && len(ips) > 0 {
			ip = ips[0]
			break
		}
		time.Sleep(5 * time.Second)
	}
	if ip == "" {
		t.Fatal("clone took no address in 3 minutes")
	}
	var pool string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		out, err := enrollGuestSSH(ip, "zpool list -H -o name 2>/dev/null")
		if err == nil && strings.TrimSpace(out) != "" {
			pool = strings.TrimSpace(out)
			break
		}
		time.Sleep(5 * time.Second)
	}
	if pool == "" {
		out, _ := enrollGuestSSH(ip, "zpool import 2>&1 | head -5; journalctl -u zfs-import-cache --no-pager | tail -3")
		t.Errorf("clone %s at %s has no imported pool:\n%s", name, ip, out)
	} else {
		t.Logf("clone pool: %s", pool)
	}
	if port := os.Getenv("VMX_CLONE_PORT"); port != "" {
		code, _ := enrollGuestSSH(ip, "curl -s -o /dev/null -w '%{http_code}' --max-time 8 http://127.0.0.1:"+port+"/")
		if strings.TrimSpace(code) != "200" {
			t.Errorf("clone service on :%s answered %q, want 200", port, strings.TrimSpace(code))
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
