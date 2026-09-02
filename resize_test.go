// resize_test.go — command construction and the refusals, both pure.
//
// The refusals matter more than the happy path here: this is the one verb that
// can destroy a filesystem if it ever runs in the wrong direction, so each
// guard is asserted by name rather than lumped into "returns an error".
package main

import "strings"

import "testing"

func row(name string, active bool, gib int) Row {
	return Row{
		D: Dom{
			Name: name, Active: active, State: map[bool]string{true: "running", false: "shut off"}[active],
			Disks: []Disk{{Target: "vda", Dev: "/dev/zvol/rpool/vms/" + name}},
			IPs:   []string{"192.168.122.50"},
		},
		Backing: "/dev/zvol/rpool/vms/" + name,
		DS:      &Dataset{Name: "rpool/vms/" + name, Type: "volume"},
	}
}

func TestResizeRefusesShrink(t *testing.T) {
	// A mounted filesystem cannot shrink and the block device under it must not.
	_, err := planResizeDisk(row("v", true, 40), 20, 40*gib)
	if err == nil {
		t.Fatal("shrinking 40G -> 20G was allowed; it must be refused")
	}
	if !strings.Contains(err.Error(), "one-way") {
		t.Fatalf("refusal should explain that growing is one-way, got: %v", err)
	}
}

func TestResizeRefusesUnknownCurrentSize(t *testing.T) {
	// Fail closed: with curBytes 0 the shrink guard cannot apply at all.
	if _, err := planResizeDisk(row("v", true, 40), 50, 0); err == nil {
		t.Fatal("a resize with an unknown current size must be refused, not assumed")
	}
}

func TestResizeRefusesStoppedGuest(t *testing.T) {
	// Layers 2 and 3 need a live kernel; a block-only resize is the half-done
	// state the verb exists to prevent.
	_, err := planResizeDisk(row("v", false, 40), 50, 40*gib)
	if err == nil {
		t.Fatal("resizing a stopped guest was allowed")
	}
	if !strings.Contains(err.Error(), "start it first") {
		t.Fatalf("refusal should tell the operator to start the guest, got: %v", err)
	}
}

func TestResizeRefusesNoAddress(t *testing.T) {
	r := row("v", true, 40)
	r.D.IPs = nil
	if _, err := planResizeDisk(r, 50, 40*gib); err == nil {
		t.Fatal("a guest with no address must be refused: the guest half runs over ssh")
	}
}

func TestResizeRefusesSameSize(t *testing.T) {
	if _, err := planResizeDisk(row("v", true, 40), 40, 40*gib); err == nil {
		t.Fatal("resizing to the size it already is must be refused")
	}
}

func TestResizePlanOrderAndContent(t *testing.T) {
	p, err := planResizeDisk(row("w1", true, 40), 50, 40*gib)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.cmds) != 3 {
		t.Fatalf("expected 3 commands (volsize, blockresize, guest), got %d:\n%s", len(p.cmds), p.cmdLines())
	}
	// Order is load bearing: the device must grow before the guest is told, and
	// the guest must be told before its partition is grown.
	if got := strings.Join(p.cmds[0], " "); !strings.Contains(got, "volsize=50G") {
		t.Errorf("first command should set volsize, got %q", got)
	}
	if got := strings.Join(p.cmds[1], " "); !strings.Contains(got, "blockresize") ||
		!strings.Contains(got, "vda") || !strings.Contains(got, "50G") {
		t.Errorf("second command should blockresize vda to 50G, got %q", got)
	}
	if got := strings.Join(p.cmds[2], " "); !strings.Contains(got, "ssh") ||
		!strings.Contains(got, "root@192.168.122.50") {
		t.Errorf("third command should ssh into the guest, got %q", got)
	}
	if !p.needsRoot {
		t.Error("a zvol resize mutates ZFS and needs root")
	}
	if !strings.Contains(p.warn, "one-way") {
		t.Errorf("the warning must say the resize cannot be undone, got %q", p.warn)
	}
}

func TestGrowRootScriptCoversEachFilesystem(t *testing.T) {
	// The guest half is chosen by FILESYSTEM, not by distro — a Fedora cloud
	// image is btrfs and a Debian one is ext4, and the same distro ships
	// different roots across images.
	for _, want := range []string{"resize2fs", "xfs_growfs", "btrfs filesystem resize max", "growpart"} {
		if !strings.Contains(growRootScript, want) {
			t.Errorf("growRootScript does not handle %q", want)
		}
	}
	// It travels as one shell-quoted ssh argument; a backtick would end the Go
	// raw string and a here-document would not survive the quoting.
	if strings.Contains(growRootScript, "`") {
		t.Error("growRootScript must not contain a backtick")
	}
}
