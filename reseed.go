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
	"regexp"
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
	// No cdrom on the source means there is nothing to RE-seed — but it also
	// means the clone has no way to learn who it is. Attach a fresh one
	// instead of giving up.
	//
	// WHY: klab's desktop goldens carry no seed (klab-desktop-centos has zero
	// cdrom devices), so every clone booted with the image's built-in
	// identity. Measured 2026-08-18: three clones of that golden all reported
	// host=localhost.localdomain, took DHCP leases that registered NO
	// hostname, and were therefore invisible to the estate view — which
	// derives membership from those leases. They were running and
	// indistinguishable, which is worse than failing.
	//
	// A target of "" tells the attach path below to add a device rather than
	// swap the media in an existing one.
	attachNew := target == ""
	if attachNew {
		target = "sdb" // first free slot on the SCSI/SATA bus for a cdrom
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
	//
	// change-media swaps the disc in a drive that already exists; attach-disk
	// adds the drive itself. A source with no cdrom needs the latter, or
	// change-media fails with "no such device".
	var upd []string
	if attachNew {
		// --type cdrom is read-only in libvirt by construction, so no --mode
		// flag: its accepted values are not documented in `virsh help` and an
		// unverified flag value is not worth the risk in a path that decides
		// whether a clone gets an identity.
		upd = append(virsh(), "attach-disk", dom, seed, target,
			"--type", "cdrom", "--config")
	} else {
		upd = append(virsh(), "change-media", dom, target, seed, "--config", "--update")
	}
	if out, err := exec.Command(upd[0], upd[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("attach seed to %s: %v: %s", dom, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// logPathRe matches the per-VM console log in a domain's XML — the sink
// libvirt opens exclusively for as long as the guest runs.
//
// The element is <log file='…'/> inside <serial>/<console>, NOT
// <source path='…'/>. Checked against real dumpxml output on 2026-08-17
// after the first version of this matched nothing and silently retargeted
// nothing, which looks identical to working.
var logPathRe = regexp.MustCompile(`(<log file=')([^']*/)[^'/]+(-console\.log')`)

// RetargetCloneLogs points a clone's console log at its own file.
//
// WHY: virt-clone carries by-reference paths from the source, and the seed
// cdrom was not the only one. A clone of klab-desktop-fedora inherits
// /var/log/klab/klab-desktop-fedora-console.log verbatim, so the source and
// every clone of it share a single file. libvirt opens that log EXCLUSIVELY,
// so the first domain to start wins and the rest fail with
//
//	Cannot open log file: '…-console.log': Device or resource busy
//
// Observed 2026-08-17: two clones stamped off klab-desktop-fedora, both
// pointing at the source's log, and the operator got "failed to start".
//
// Args:    dom — the clone's domain name.
// Returns: nil when the clone has its own log path, or when it has no
//
//	file-backed console log at all (nothing to retarget).
//
// Failure modes callers must handle: dumpxml or define being rejected.
func RetargetCloneLogs(dom string) error {
	out, err := virshOut("dumpxml", dom)
	if err != nil {
		return fmt.Errorf("dumpxml %s: %w", dom, err)
	}
	x := string(out)

	fixed := logPathRe.ReplaceAllString(x, "${1}${2}"+dom+"${3}")
	if fixed == x {
		return nil // no file-backed console log, or it is already ours
	}

	// Redefine from a temp file rather than stdin: virsh define takes a path,
	// and a domain XML is comfortably past what is safe to pass as an argument.
	tmp, err := os.CreateTemp("", "vmx-relog-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(fixed); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	// The directory has to exist and be writable by libvirt before the guest
	// starts, or the retarget just moves the failure rather than fixing it.
	for _, m := range logPathRe.FindAllStringSubmatch(fixed, -1) {
		if d := strings.TrimSuffix(m[2], "/"); d != "" {
			// Best effort: the directory usually already exists, having been
			// made for the source. A failure here surfaces as a start error
			// naming the path, which is clearer than anything raised now.
			_ = exec.Command("sudo", "-n", "install", "-d", "-m0755", d).Run()
		}
	}

	def := append(virsh(), "define", tmp.Name())
	if out, err := exec.Command(def[0], def[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("redefine %s with its own console log: %v: %s",
			dom, err, strings.TrimSpace(string(out)))
	}
	return nil
}
