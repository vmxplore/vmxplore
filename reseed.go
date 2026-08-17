// reseed.go — give a clone its own cloud-init identity.
//
// What it does, in order:
//  1. Finds the cdrom the clone inherited from the golden it came off.
//  2. Builds a fresh NoCloud seed carrying the CLONE's hostname and a
//     unique instance-id.
//  3. Swaps the cdrom to the new seed, in the persistent config.
//
// WHY THIS EXISTS: `virt-clone --preserve-data --file <zvol>` remaps only the
// FIRST disk. The golden's second device — its cloud-init seed cdrom — is
// carried into every clone by reference, so all of them boot the golden's
// user-data and come up claiming the golden's identity.
//
// Measured on a live host, 2026-08-17: three clones of golden-dr827w2yhf, all
// three pointing at golden-dr827w2yhf-seed.iso, and `hostnamectl --static`
// inside each returning "golden-dr827w2yhf". Their DHCP leases carried no
// distinguishable hostname, the estate could not tell them apart, and none of
// them registered as a distinct peer on the mesh.
//
// Sealing does not solve this. kldload-seal clears machine-id, hostid and the
// ssh host keys INSIDE the image, so cloud-init runs again on every clone —
// but it runs against the same seed, and sets the same hostname again. The
// identity has to change on the OUTSIDE, which is what this does.
//
// Inputs:  the clone's domain name, and the spec fields that must survive
//
//	from the golden (user, password, ssh key).
//
// Outputs: a seed at /var/lib/libvirt/images/<clone>-seed.iso, and the
//
//	clone's cdrom pointed at it.
//
// Notes:
//   - instance-id is derived from the clone name, so cloud-init treats the
//     first boot as a first boot and applies the new hostname. Reusing the
//     golden's id makes cloud-init skip the run entirely as already-done.
//   - Failure is returned, never swallowed: a clone with the wrong identity
//     looks fine until two of them are on the network together.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cloneSeedDir is where libvirt can actually read a seed from. It matches
// BuildNewVM's choice; a seed anywhere else is unreadable by qemu under
// SELinux/AppArmor even when the path is correct.
const cloneSeedDir = "/var/lib/libvirt/images"

// cdromTarget returns the target device name of a domain's cdrom, e.g. "sda".
//
// Returns "" and no error when the domain has no cdrom — a clone of a VM that
// was never cloud-init seeded is a legitimate case and needs no reseed.
func cdromTarget(dom string) (string, error) {
	out, err := virshOut("domblklist", dom, "--details")
	if err != nil {
		return "", fmt.Errorf("domblklist %s: %w", dom, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// Type Device Target Source
		if len(f) >= 4 && f[1] == "cdrom" {
			return f[2], nil
		}
	}
	return "", nil
}

// ReseedClone gives dom its own cloud-init seed.
//
// Args:    dom   the clone's domain name, which becomes its hostname
//
//	spec  identity fields inherited from the golden (User, Password,
//	      SSHKey). Name and Desktop are overridden here: the hostname
//	      must be the clone's, and a desktop is already installed in
//	      the image so re-running its recipe would be a long no-op.
//
// Returns: nil when the clone now boots its own identity.
// Failure modes callers must handle: no cdrom (returns nil, nothing to do),
// no iso builder on the host, and a virsh update that is rejected.
func ReseedClone(dom string, spec NewVMSpec) error {
	target, err := cdromTarget(dom)
	if err != nil {
		return err
	}
	if target == "" {
		return nil // nothing seeded this VM; nothing to re-seed
	}

	// The clone's own identity. Desktop is deliberately cleared — the recipe
	// ran on the golden and the packages are in the image, and leaving it set
	// would also re-arm cloud-init's power_state reboot on every clone.
	s := spec
	s.Name = dom
	s.Desktop = ""

	tmp, err := os.MkdirTemp("", "vmx-reseed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	ud := filepath.Join(tmp, "user-data")
	md := filepath.Join(tmp, "meta-data")
	if err := os.WriteFile(ud, []byte(userData(s)), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(md, []byte(metaData(dom)), 0600); err != nil {
		return err
	}

	tmpISO := filepath.Join(tmp, dom+"-seed.iso")
	argv, err := isoTool(tmpISO, ud, md)
	if err != nil {
		return err
	}
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("build seed for %s: %v: %s", dom, err,
			strings.TrimSpace(string(out)))
	}

	// install -m0600, matching BuildNewVM: the seed carries a hashed password
	// and the operator's ssh key, and 0644 under a world-readable directory
	// hands both to every local account.
	seed := filepath.Join(cloneSeedDir, dom+"-seed.iso")
	if out, err := exec.Command("sudo", "-n", "install", "-m0600",
		tmpISO, seed).CombinedOutput(); err != nil {
		return fmt.Errorf("install seed for %s: %v: %s", dom, err,
			strings.TrimSpace(string(out)))
	}

	// --config, not --live: the clone is defined and shut off at this point,
	// and the change has to be in the persistent XML that its first boot
	// reads. A --live update against a stopped domain is an error.
	upd := append(virsh(), "change-media", dom, target, seed, "--config", "--update")
	if out, err := exec.Command(upd[0], upd[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("attach seed to %s: %v: %s", dom, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}
