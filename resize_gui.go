//go:build gui

// resize_gui.go — the one part of the resize verb that reads the hypervisor.
//
// Split out of resize.go on purpose. resize.go is pure command construction and
// compiles in BOTH flavors so resize_test.go can exercise it; currentDiskBytes
// shells out and is only ever called from the GUI dialog, which is itself
// behind `//go:build gui`. Left in the untagged file it was dead code in the
// non-GUI build, and staticcheck's U1000 said so — after the local go
// build/vet/test all passed, because none of those look for unused functions.
// (vmxplore CI run 58, 2026-09-02.)
package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// currentDiskBytes reads the disk's virtual size from the hypervisor.
//
// Impure by necessity, and kept out of planResizeDisk so command construction
// stays testable. Returns 0 with an error when the size cannot be established —
// which planResizeDisk treats as a refusal, not as "assume zero", because the
// shrink guard is only as good as this number.
func currentDiskBytes(r Row) (uint64, error) {
	if r.DS != nil && r.DS.Type == "volume" {
		argv := zfsArgv("get", "-Hp", "-o", "value", "volsize", r.DS.Name)
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			return 0, fmt.Errorf("zfs get volsize %s: %w", r.DS.Name, err)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("volsize of %s is not a number: %q", r.DS.Name, strings.TrimSpace(string(out)))
		}
		return n, nil
	}
	if r.Backing == "" {
		return 0, fmt.Errorf("%s has no disk", r.D.Name)
	}
	// File-backed: ask qemu-img for the VIRTUAL size, which is the number the
	// guest sees. The file's size on disk is smaller for a sparse qcow2 and
	// would make every resize look like a shrink.
	q := []string{"qemu-img", "info", "--output=json", r.Backing}
	if target.SSHHost != "" {
		q = sshArgv(target.SSHHost, q...)
	}
	out, err := exec.Command(q[0], q[1:]...).Output()
	if err != nil {
		return 0, fmt.Errorf("qemu-img info %s: %w", r.Backing, err)
	}
	var info struct {
		VirtualSize uint64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Errorf("qemu-img info %s: %w", r.Backing, err)
	}
	if info.VirtualSize == 0 {
		return 0, fmt.Errorf("qemu-img reported a zero virtual size for %s", r.Backing)
	}
	return info.VirtualSize, nil
}
