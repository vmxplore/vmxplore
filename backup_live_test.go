package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestBackupRestoreLiveVM backs up a real, shut-off VM, restores it as a new
// one, checks libvirt accepts the definition, and removes everything it made.
// Opt-in: VMX_LIVE_BACKUP=<vm>. It owns only: rpool/vmxbk-livetest, the VM
// <vm>-bktest and its zvols, and the backup-* snapshot it leaves on <vm>'s
// disks. The VM's own disks are read, never written.
func TestBackupRestoreLiveVM(t *testing.T) {
	vm := os.Getenv("VMX_LIVE_BACKUP")
	if vm == "" {
		t.Skip("set VMX_LIVE_BACKUP=<shut-off vm> to run against a real VM")
	}
	ctx := context.Background()
	newVM := vm + "-bktest"
	dest := backupDest{Dataset: "rpool/vmxbk-livetest"}
	logf := func(s string) { t.Log(s) }

	xml, err := run(ctx, nil, virsh("dumpxml", "--inactive", vm))
	if err != nil {
		t.Fatalf("dumpxml: %v", err)
	}
	if state, _ := run(ctx, nil, virsh("domstate", vm)); state != "shut off" {
		t.Skipf("%s is %q; this test only reads a shut-off VM", vm, state)
	}
	var disks []string
	for _, l := range strings.Split(xml, "\n") {
		if i := strings.Index(l, "/dev/zvol/"); i >= 0 {
			p := l[i+len("/dev/zvol/"):]
			if j := strings.IndexAny(p, `'"`); j > 0 {
				disks = append(disks, p[:j])
			}
		}
	}
	if len(disks) == 0 {
		t.Fatalf("%s has no zvol disks", vm)
	}
	parent := disks[0][:strings.LastIndex(disks[0], "/")]
	var point string
	t.Cleanup(func() {
		bg := context.Background()
		if _, err := run(bg, nil, virsh("undefine", "--nvram", newVM)); err != nil {
			_, _ = run(bg, nil, virsh("undefine", newVM)) // no nvram on a BIOS guest
		}
		for _, d := range disks {
			nd := parent + "/" + restoredDiskName(base(d), vm, newVM)
			if exists(bg, srcZFS, nd) {
				if _, err := run(bg, nil, srcZFS("destroy", "-r", nd)); err != nil {
					t.Errorf("cleanup could not remove %s: %v", nd, err)
				}
			}
			if point != "" && exists(bg, srcZFS, d+"@"+point) {
				_, _ = run(bg, nil, srcZFS("destroy", d+"@"+point))
			}
		}
		if exists(bg, srcZFS, dest.Dataset) {
			_, _ = run(bg, nil, srcZFS("destroy", "-r", dest.Dataset))
		}
	})
	if exists(ctx, srcZFS, dest.Dataset) {
		t.Fatalf("%s already exists; refusing to use it", dest.Dataset)
	}

	point, err = BackupDisks(ctx, vm, disks, xml, dest, 2, logf)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	sets, err := ListBackups(ctx, dest)
	if err != nil || len(sets) != 1 || sets[0].VM != vm || sets[0].Points[0] != point {
		t.Fatalf("ListBackups = %+v, %v", sets, err)
	}
	newXML, made, err := RestoreDisks(ctx, dest, vm, point, parent, newVM, logf)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	f, err := os.CreateTemp("", "vmxbk-*.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(newXML); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := run(ctx, nil, virsh("define", f.Name())); err != nil {
		t.Fatalf("libvirt refused the restored definition: %v %s", err, out)
	}
	got, err := run(ctx, nil, virsh("dumpxml", "--inactive", newVM))
	if err != nil {
		t.Fatalf("dumpxml %s: %v", newVM, err)
	}
	for _, m := range made {
		if !strings.Contains(got, "/dev/zvol/"+m) {
			t.Errorf("%s does not use the restored disk %s", newVM, m)
		}
	}
	for _, d := range disks {
		if strings.Contains(got, "/dev/zvol/"+d+"'") || strings.Contains(got, "/dev/zvol/"+d+`"`) {
			t.Errorf("%s still points at the original disk %s", newVM, d)
		}
	}
	origUUID, _ := run(ctx, nil, virsh("domuuid", vm))
	newUUID, _ := run(ctx, nil, virsh("domuuid", newVM))
	if origUUID == "" || origUUID == newUUID {
		t.Errorf("restored VM shares the original's UUID (%q)", newUUID)
	}
	for _, m := range made {
		if snaps, _ := backupSnaps(ctx, srcZFS, m); len(snaps) != 0 {
			t.Errorf("restored disk %s still carries %v; it should start clean", m, snaps)
		}
	}
	t.Logf("restored %s as %s from %s; disks %v", vm, newVM, point, made)
}
