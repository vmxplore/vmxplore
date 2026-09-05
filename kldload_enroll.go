// kldload_enroll.go — on a kldload host, an appliance is born enrolled.
//
// WHAT IT DOES, IN ORDER, after BuildNewVM has the guest running:
//  1. Waits for the guest to take an address and answer root SSH (the build
//     seeded the host's ops key into root's authorized_keys via cloud-init).
//  2. Puts the VM on its own WireGuard mesh via kvm-mesh — one mesh per
//     appliance, named after it, on the first free 10.254.x/24. wgxplore's
//     inventory is derived from live peer endpoints, so the new group appears
//     there on the next sync without anyone maintaining a list.
//  3. Issues a TLS leaf for the VM from the per-install kldload CA
//     (kldload-ca issue) and stages it in the guest at /etc/kldload/tls/,
//     with the estate root installed into the guest's trust store — so a
//     recipe that serves HTTPS can point at server.crt/server.key and every
//     kldload machine already trusts it.
//  4. Registers the VM in the state DB (kldload-db vm-register), which is
//     what the Ansible dynamic inventory reads.
//
// WHY: the tiles' pitch is not "a VM with software in it" — it is that on a
// kldload host the appliance joins the substrate: its own network plane, a
// cert the estate trusts, a row in the inventory. On a plain KVM or KVM+ZFS
// host every step here is skipped and the appliance still works; that is the
// three-tier story (kldload / kvm+zfs / kvm), not a failure.
//
// Every step is additive and best-effort BY DESIGN: a mesh that could not
// come up must not destroy a working media server. Failures are logged with
// the command to retry, never swallowed silently.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// enrollNameRE gates every name this file passes to a privileged argv.
// kvm-mesh interface names are capped at 15 by the kernel; the mesh name IS
// the interface name, so the cap is enforced at derivation below.
var enrollNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,30}$`)

func haveHostCmd(c string) bool { _, err := exec.LookPath(c); return err == nil }

// KldloadTier reports what the LOCAL host offers an appliance build.
//
//	kldload  — kvm-mesh + kldload-ca present: full enrollment
//	kvm+zfs  — a pool for the data disk, no estate services
//	kvm      — plain libvirt; recipes degrade to directories
//
// Remote targets are "kvm" for now: enrollment shells out to host tools with
// local paths, and doing that through a jump host is a feature, not a v1.
func KldloadTier() string {
	if target.SSHHost != "" {
		return "kvm"
	}
	if haveHostCmd("kvm-mesh") && haveHostCmd("kldload-ca") {
		return "kldload"
	}
	if HasZFS() {
		return "kvm+zfs"
	}
	return "kvm"
}

// sudoRun executes one privileged host command with a fixed argv.
func sudoRun(args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// hostOpsPubkey returns the host's root ops public key, or "".
// This is the key kvm-mesh and the cert push use to reach guests as root;
// seeding it at build time is what makes the guest enrollable at all.
func hostOpsPubkey() string {
	for _, p := range []string{"/root/.ssh/id_ed25519.pub", "/root/.ssh/id_kldload.pub"} {
		if out, err := sudoRun("cat", p); err == nil && strings.HasPrefix(out, "ssh-") {
			// First line only — a trailing shell warning must not become
			// part of an authorized_keys entry.
			return strings.SplitN(out, "\n", 2)[0]
		}
	}
	return ""
}

// enrollMeshName derives the appliance's mesh (= interface) name: "ap-" plus
// the VM name, truncated to the kernel's 15-char interface cap.
func enrollMeshName(vm string) string {
	m := "ap-" + strings.ToLower(vm)
	if len(m) > 15 {
		m = m[:15]
	}
	return strings.TrimRight(m, "-")
}

// meshStateDir is kvm-mesh's MESH_DIR: one <mesh>.members file per mesh,
// world-readable, so an unprivileged console can ask "was this VM ever
// enrolled?" without a sudo prompt.
const meshStateDir = "/var/lib/kldload/mesh"

// meshPresent reports whether kvm-mesh holds any state for the mesh — its
// members file, or the interface itself. Either alone is enough to want a
// `down`: a file with no interface is what a reboot leaves behind (onyx had
// three of those), an interface with no file is a teardown that was cut off.
func meshPresent(mesh string) bool {
	if _, err := os.Stat(filepath.Join(meshStateDir, mesh+".members")); err == nil {
		return true
	}
	_, err := os.Stat("/sys/class/net/" + mesh)
	return err == nil
}

// meshTeardownPost is the delete verb's mesh cleanup: the argv list that takes
// vm's ap-* mesh down and resyncs the host's network view. Empty when there is
// nothing to do — the target is remote (the mesh lives on the hypervisor, and
// this runs here), the host has no kvm-mesh, or the VM was never enrolled — so
// a plain KVM host never sees a sudo prompt for it.
func meshTeardownPost(vm string) [][]string {
	if target.SSHHost != "" || !haveHostCmd("kvm-mesh") {
		return nil
	}
	mesh := enrollMeshName(vm)
	if !meshPresent(mesh) {
		return nil
	}
	return meshTeardownArgv(mesh, os.Geteuid() == 0, haveHostCmd("kldload-networks"))
}

// meshTeardownArgv is the pure half of meshTeardownPost, split out so the
// command shape is unit-testable on a host with no mesh at all.
func meshTeardownArgv(mesh string, root, haveNetworks bool) [][]string {
	out := [][]string{asRoot(root, "kvm-mesh", "down", mesh)}
	if haveNetworks {
		out = append(out, asRoot(root, "kldload-networks", "sync"))
	}
	return out
}

// enrollGuestSSH runs one command in the guest as root, with the same policy
// the resize verb uses for guests (no host-key pinning: clones churn).
func enrollGuestSSH(ip string, cmd string) (string, error) {
	argv := append([]string{"ssh"}, guestSSHFlags...)
	argv = append(argv, "root@"+ip, cmd)
	return sudoRun(argv...)
}

// enrollHostMu serialises the host-side enrollment steps across parallel
// builds. allocMeshSubnet reads `ip addr` and returns the first free
// 10.254.x; the subnet only becomes visibly used once `kvm-mesh up` has
// created the interface. Two enrollments between those two moments pick
// the same /24, and the second `up` either fails or lands two meshes on
// one subnet. Build-all can run tiles in parallel (VMX_BUILD_JOBS), so the
// pick-and-create pair holds this lock; the CA issue does too, since the
// estate CA keeps a serial index file. Everything else in enrollment is
// ssh into one guest and needs no ordering.
var enrollHostMu sync.Mutex

// allocMeshSubnet picks the /24 for a mesh: the one it already has when the
// interface exists (re-enrolling is how an operator repairs), else the first
// free 10.254.x. Returned as the first three octets. Callers hold
// enrollHostMu through the `kvm-mesh up` that makes the pick real.
func allocMeshSubnet(mesh string) (string, error) {
	if out, err := sudoRun("ip", "-o", "-4", "addr", "show", mesh); err == nil && out != "" {
		f := strings.Fields(out)
		for i := range f {
			if f[i] == "inet" && i+1 < len(f) {
				oct := strings.Split(strings.SplitN(f[i+1], "/", 2)[0], ".")
				if len(oct) == 4 {
					return strings.Join(oct[:3], "."), nil
				}
			}
		}
	}
	used, _ := sudoRun("ip", "-o", "-4", "addr")
	for n := 40; n <= 250; n++ {
		if !strings.Contains(used, fmt.Sprintf(" 10.254.%d.", n)) &&
			!strings.Contains(used, fmt.Sprintf("inet 10.254.%d.", n)) {
			return fmt.Sprintf("10.254.%d", n), nil
		}
	}
	return "", fmt.Errorf("no free 10.254.x/24 left")
}

// EnrollAppliance runs the whole enrollment. Never returns an error — each
// step degrades to a logged warning, because a half-enrolled appliance that
// WORKS beats a destroyed build. appSlug tags the inventory role.
func EnrollAppliance(vmName, appSlug string, log func(string)) {
	_ = EnrollDomain(vmName, "appliance-"+appSlug, log)
}

// EnrollDomain enrolls a libvirt domain by name: the address through
// libvirt's leases, then EnrollGuestAt. role "" keeps whatever role the
// inventory already has for it — a clone stays a clone. Used for the
// clones a Clone / Fleet run starts and for the "Enroll on the substrate"
// verb; before this only a built appliance was ever enrolled, and a clone
// of one was on the estate without a mesh or a cert ("i want the full
// meal deal", operator, 2026-09-05).
func EnrollDomain(vmName, role string, log func(string)) error {
	if KldloadTier() != "kldload" {
		return nil
	}
	if !enrollNameRE.MatchString(vmName) {
		log("enroll: VM name " + vmName + " fails the safety pattern — skipped")
		return fmt.Errorf("enroll: unsafe name %q", vmName)
	}

	// ── wait for an address, then for root SSH (cloud-init must land the
	// ops key before anything below can work) ──
	var ip string
	lv, err := ConnectSystem()
	if err != nil {
		log("enroll: no libvirt connection — skipped")
		return err
	}
	defer lv.Close()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if ips, err := lv.LeaseIPs(vmName); err == nil && len(ips) > 0 {
			ip = ips[0]
			break
		}
		time.Sleep(5 * time.Second)
	}
	if ip == "" {
		log("enroll: " + vmName + " took no address in 4m — run enrollment later by hand")
		return fmt.Errorf("enroll: %s took no address", vmName)
	}
	return EnrollGuestAt(vmName, ip, role, log)
}

// EnrollGuestAt is enrollment from the address on: root ssh, the recipe's
// end, the mesh, the estate cert, the inventory row. EnrollAppliance finds
// the address through libvirt and calls this; `vmx --enroll` calls it for
// a guest that has no domain — a Firecracker microVM, whose address kfire
// already knows. One implementation, so a microVM ends up on the substrate
// the same way a built appliance does ("do the clones and microVMs also
// need/have wireguard?", operator, 2026-09-05). Returns an error only when
// nothing could be done at all (no root ssh); partial steps are logged.
func EnrollGuestAt(vmName, ip, role string, log func(string)) error {
	if KldloadTier() != "kldload" {
		return nil
	}
	if !enrollNameRE.MatchString(vmName) {
		log("enroll: VM name " + vmName + " fails the safety pattern — skipped")
		return fmt.Errorf("enroll: unsafe name %q", vmName)
	}
	log("enroll: " + vmName + " at " + ip + " — waiting for root ssh")
	sshUp := false
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := enrollGuestSSH(ip, "true"); err == nil {
			sshUp = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !sshUp {
		log("enroll: root ssh never answered — the guest may predate key seeding; skipped")
		return fmt.Errorf("enroll: no root ssh to %s at %s", vmName, ip)
	}

	// ── mesh ──
	// First let the recipe finish. WaitAppliance returns when the tile's
	// port answers, and on the Debian tiles that is minutes before cloud-init
	// is done: tvheadend's port was up at 11:51:36 and its cloud-init
	// finished at 11:51:38; WriteFreely's enrollment ran 90 s before its
	// recipe ended (onyx, 2026-09-04, the first parallel build-all). The
	// wireguard-tools install below then met the recipe's own apt holding
	// the dpkg lock, failed under its 2>/dev/null, and kvm-mesh refused with
	// "no 'wg' in the guest" — an empty members file and no mesh, on tiles
	// whose recipes had passed. cloud-init status --wait blocks until every
	// module has run; bounded, because a recipe that never finishes is a
	// different problem and enrollment must still report it.
	log("enroll: waiting for " + vmName + "'s first boot to finish before touching its packages")
	if _, err := enrollGuestSSH(ip,
		"command -v cloud-init >/dev/null 2>&1 && timeout 900 cloud-init status --wait >/dev/null 2>&1; true"); err != nil {
		log("enroll: could not wait on cloud-init in " + vmName + " — continuing")
	}
	// kvm-mesh mints the guest's key IN the guest, which needs wg there.
	// Stock cloud images do not carry it; the first enrollment failed with
	// "no 'wg' in the guest — install wireguard-tools there first", so do
	// exactly that, family-aware, before asking.
	_, _ = enrollGuestSSH(ip,
		"command -v wg >/dev/null 2>&1 || dnf -y install wireguard-tools >/dev/null 2>&1 || "+
			"(apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq wireguard-tools) >/dev/null 2>&1 || true")
	mesh := enrollMeshName(vmName)
	enrollHostMu.Lock()
	if subnet, err := allocMeshSubnet(mesh); err != nil {
		log("enroll: mesh skipped — " + err.Error())
	} else if out, err := sudoRun("kvm-mesh", "up", mesh, subnet, vmName); err != nil {
		log("enroll: kvm-mesh up failed — retry with: sudo kvm-mesh up " +
			mesh + " " + subnet + " " + vmName)
		if out != "" {
			log("enroll:   " + strings.SplitN(out, "\n", 2)[0])
		}
	} else {
		log("enroll: mesh '" + mesh + "' up on " + subnet + ".0/24 — wgxplore picks it up on next sync")
	}
	enrollHostMu.Unlock()

	// ── TLS leaf from the estate CA ──
	issue := func() (string, error) {
		enrollHostMu.Lock()
		defer enrollHostMu.Unlock()
		return sudoRun("kldload-ca", "issue", vmName, "--dns", vmName, "--ip", ip)
	}
	if _, err := sudoRun("test", "-f", "/etc/kldload/ca/root/ca.crt"); err != nil {
		log("enroll: no estate CA root on this host — cert step skipped")
	} else if out, err := issue(); err != nil {
		log("enroll: kldload-ca issue failed — " + strings.SplitN(out, "\n", 2)[0])
	} else {
		crt := "/etc/kldload/ca/leaves/" + vmName + ".crt"
		key := "/etc/kldload/ca/leaves/" + vmName + ".key"
		if _, err := enrollGuestSSH(ip, "install -d -m 0750 /etc/kldload/tls"); err != nil {
			log("enroll: could not create /etc/kldload/tls in the guest")
		} else {
			push := func(src, dst, mode string) bool {
				argv := append([]string{"scp"}, guestSSHFlags...)
				argv = append(argv, src, "root@"+ip+":"+dst)
				if _, err := sudoRun(argv...); err != nil {
					return false
				}
				_, err := enrollGuestSSH(ip, "chmod "+mode+" "+dst)
				return err == nil
			}
			ok := push(crt, "/etc/kldload/tls/server.crt", "0644") &&
				push(key, "/etc/kldload/tls/server.key", "0600") &&
				push("/etc/kldload/ca/root/ca.crt", "/etc/kldload/tls/ca.crt", "0644")
			if ok {
				// Install the estate root into the guest's own trust store,
				// family-aware, so the guest also TRUSTS its neighbours.
				_, _ = enrollGuestSSH(ip,
					"if command -v update-ca-trust >/dev/null 2>&1; then "+
						"cp /etc/kldload/tls/ca.crt /etc/pki/ca-trust/source/anchors/kldload-estate-ca.crt && update-ca-trust extract; "+
						"elif command -v update-ca-certificates >/dev/null 2>&1; then "+
						"cp /etc/kldload/tls/ca.crt /usr/local/share/ca-certificates/kldload-estate-ca.crt && update-ca-certificates; fi")
				log("enroll: estate cert staged at /etc/kldload/tls/ (server.crt, server.key, ca.crt)")
			} else {
				log("enroll: cert push incomplete — files are on the host under /etc/kldload/ca/leaves/")
			}
		}
	}

	// ── inventory ──
	if haveHostCmd("kldload-db") {
		args := []string{"kldload-db", "vm-register", "--name", vmName, "--status", "running"}
		if role != "" {
			args = append(args, "--role", role)
		} else {
			role = "its existing role"
		}
		if _, err := sudoRun(args...); err != nil {
			log("enroll: kldload-db vm-register failed — the VM will not appear in Ansible")
		} else {
			log("enroll: registered in the estate inventory as " + role)
		}
	}
	log("enroll: done — " + vmName + " is on the substrate")
	return nil
}

// applianceSlug is the inventory-safe form of a catalog name.
func applianceSlug(name string) string {
	s := strings.ToLower(name)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
	return strings.Trim(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '-' }), "-"), "-")
}
