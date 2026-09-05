// ─────────────────────────────────────────────────────────────────────────────
// capabilities.go — what THIS host can actually do, and why a tile is grey.
//
// What it does, in order:
//  1. Probes the three things every VM capability in vmx rests on: a working
//     KVM, ZFS, and the kldload k-tools.
//  2. Folds those probes into a Tier, purely for display — one line telling the
//     operator which world they are in.
//  3. Answers, per capability, "can this host do it, and if not, what is the
//     one sentence that explains it" — which is what greys a tile.
//
// WHY IT EXISTS: vmx is not a kldload-only tool. It ships standalone and lands
// on plain libvirt boxes, on ZFS-on-root boxes that never heard of kldload, and
// on full kldload installs. Before this, every image and clone tile was offered
// unconditionally, so a plain-KVM user could press "images: base…" and watch
// klab fail on a zvol path that was never going to exist. An option you cannot
// use should say so before you press it, not after.
//
// THE DESIGN RULE THIS OBEYS (rules.go, "kldload detection"): detection gates
// cosmetic flair and the default ruleset ONLY — never whether a capability
// exists. Capabilities gate on probes. So Tier() is for the status line and
// nothing branches on it; every gate below asks the specific question it needs
// (is there a zfs binary? is klab on PATH?). build-iso.sh once gated behaviour
// on a profile NAME and shipped a profile that silently lacked the thing the
// name promised; this file is deliberately built the other way round.
//
// Inputs:  /dev/kvm, PATH (zfs, klab, virsh), and target.SSHHost for remotes.
// Outputs: pure functions, no state, no side effects. Safe to call per repaint.
//
// Notes:
//   - Remote (ssh) targets report capable rather than probing. The estate read
//     already succeeded over ssh, and a second round trip per tile per repaint
//     would be paid on every frame. Same reasoning HasZFS already uses.
//   - "kldload" is NOT a superset flag. A kldload host is KVM+ZFS+k-tools, but
//     a hand-built ZFS+libvirt box that installed klab is equally capable, and
//     it gets the same tiles. The tier is a label, not a permission.
// ─────────────────────────────────────────────────────────────────────────────

package main

import (
	"os"
	"os/exec"
)

// Capability is one thing a tile may require. Tiles declare what they need;
// this file decides whether the host has it.
type Capability int

const (
	// CapNone — always available. Reads, help text, anything host-agnostic.
	CapNone Capability = iota
	// CapKVM — needs a hypervisor: create, start, console, cloud/local images.
	CapKVM
	// CapZFS — needs ZFS on the hypervisor: instant clones, zvol snapshots.
	CapZFS
	// CapKlab — needs the kldload golden-image toolchain (klab) AND ZFS under
	// it, because every golden klab builds is a zvol under ${POOL}/vms.
	CapKlab
	// CapDB — needs the kldload state database (kldload-db). This is the one
	// capability that is not about hardware or storage: it is a shared
	// INVENTORY, and it unlocks things the other tiers cannot do at any price.
	//
	// node-register already records --wg-pubkey and --ip-wg0 per node, so a
	// host that can read the DB can enumerate its peers' WireGuard identities
	// without anyone typing a key. Same for cluster membership, VM→cluster
	// grouping, and deployment history. None of that is inferable from
	// libvirt or ZFS, however capable the host is otherwise.
	CapDB
)

// HasStateDB reports whether the kldload inventory is readable here.
//
// LookPath only — actually running `kldload-db dump` is a subprocess, and this
// is called per tile per repaint. reconcile.go does the real read once and
// degrades to "no annotation" if the schema moved; this just decides whether
// to offer the tile.
func HasStateDB() bool {
	if target.SSHHost != "" {
		return true
	}
	return kldloadDB() != ""
}

// TierFeature is one row of the capability comparison: what a thing is, and
// which of the three worlds can do it.
//
// WHY IT IS A TABLE AND NOT PROSE: the honest answer to "what do I lose
// without ZFS" is per-feature, and it is not obvious — a plain-KVM host can
// absolutely clone a VM, it just pays a full disk copy for it instead of 0.2s.
// Saying "no clones" would be a lie; saying "full copy, not instant" is the
// fact. Every cell below is what the tools actually do, checked against
// kvm-clone (zfs snapshot + zfs clone), klab (zvol_path → ${POOL}/vms) and
// kldload-db (node-register --wg-pubkey/--ip-wg0), 2026-08-20.
type TierFeature struct {
	Name    string
	KVM     string // plain libvirt host
	KVMZFS  string // libvirt on ZFS
	Kldload string // the above plus klab and the state DB
}

// FeatureMatrix is the comparison shown in the capabilities view.
//
// Ordered by how early an operator meets each one, not by tier: run a machine,
// then copy one, then snapshot it, then build images, then run a fleet.
var FeatureMatrix = []TierFeature{
	{"Run VMs (create, console, lifecycle)",
		"yes", "yes", "yes"},
	{"Cloud images (distro qcow2)",
		"yes", "yes", "yes"},
	{"Local ISO installs",
		"yes", "yes", "yes"},
	{"Clone a VM",
		"full disk copy", "instant, copy-on-write", "instant, copy-on-write"},
	{"Snapshots",
		"qcow2 internal", "zvol snapshots + rollback", "zvol snapshots + rollback"},
	{"Golden image lineages (base / desktop / ztest)",
		"no", "no", "yes (klab)"},
	{"Multi-node cluster from goldens",
		"no", "no", "yes (kspawn)"},
	{"Cluster + VM inventory across hosts",
		"no", "no", "yes (state DB)"},
	{"WireGuard peers discovered, not typed",
		"no", "no", "yes (wg-pubkey in DB)"},
	{"Deployment history and events",
		"no", "no", "yes (state DB)"},
}

// HasKVM reports whether this host can run a machine at all.
//
// /dev/kvm is the honest question — virsh being installed proves nothing about
// whether the CPU exposes virtualization, and a box with libvirt but no VT-x
// silently falls back to TCG emulation, which is the "why is my VM 40x slower"
// report. Checked for readability, not mere existence: /dev/kvm present but
// unreadable (no kvm group membership) is the same outcome for the user.
func HasKVM() bool {
	if target.SSHHost != "" {
		return true
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err == nil {
		_ = f.Close()
		return true
	}
	// Fall back to libvirt: a session that can reach a hypervisor is capable
	// even where /dev/kvm is hidden from us by permissions we do not need.
	if _, err := exec.LookPath("virsh"); err == nil {
		return true
	}
	return false
}

// HasKlab reports whether the kldload golden-image toolchain is present.
//
// LookPath("klab"), not IsKldload(). A kldload install is the usual way to get
// klab, but it is not the only one, and gating image builds on "does /etc/kldload
// exist" would refuse a perfectly capable hand-built host. Ask for the tool.
func HasKlab() bool {
	if target.SSHHost != "" {
		return true
	}
	_, err := exec.LookPath("klab")
	return err == nil
}

// Available reports whether this host can do c, and when it cannot, the one
// sentence a tile shows instead of running.
//
// The reason strings name the MISSING PIECE and the consequence, not the
// abstraction. "Needs ZFS" tells an operator nothing they can act on; "clones
// are zfs snapshot + zfs clone" tells them why no amount of libvirt fixes it.
func Available(c Capability) (bool, string) {
	switch c {
	case CapNone:
		return true, ""
	case CapKVM:
		if HasKVM() {
			return true, ""
		}
		return false, "no /dev/kvm and no virsh — this host cannot run a VM"
	case CapZFS:
		if !HasZFS() {
			return false, "needs ZFS: a clone is `zfs snapshot` + `zfs clone`, so there is no non-ZFS path"
		}
		if !HasKVM() {
			return false, "needs KVM as well as ZFS"
		}
		return true, ""
	case CapKlab:
		if !HasKlab() {
			return false, "needs klab (kldload) — the golden-image builder"
		}
		if !HasZFS() {
			return false, "needs ZFS: klab builds every golden as a zvol under <pool>/vms"
		}
		if !HasKVM() {
			return false, "needs KVM to boot the image it is building"
		}
		return true, ""
	case CapDB:
		if !HasStateDB() {
			return false, "needs the kldload state DB — nothing else knows the estate"
		}
		return true, ""
	}
	return true, ""
}

// TierColumn returns which FeatureMatrix column describes THIS host, so the
// comparison view can mark the operator's own row. Display only.
func TierColumn() string {
	switch {
	case HasKlab() && HasZFS() && HasStateDB():
		return "kldload"
	case HasZFS() && HasKVM():
		return "KVM+ZFS"
	case HasKVM():
		return "KVM"
	default:
		return ""
	}
}

// Tier is the one-line summary for the status bar. DISPLAY ONLY — nothing
// branches on it. See the design-rule note in the file header.
func Tier() string {
	switch {
	case !HasKVM():
		return "no hypervisor — VM actions unavailable"
	case HasKlab() && HasZFS():
		return "kldload (KVM + ZFS + klab) — everything available"
	case HasZFS():
		return "KVM + ZFS — clones and snapshots, no golden builder"
	default:
		return "KVM only — cloud and local images; no clones, no goldens"
	}
}

// ─── Locating kldload's own tools ────────────────────────────────────
//
// kldloadDB resolves the kldload-db binary, or "" when it is genuinely absent.
//
// WHY THIS IS NOT JUST LookPath: kldload installs the tool to
// /usr/local/sbin, and a desktop session's PATH does not include sbin
// directories. Every caller here used LookPath alone and treated the miss as
// "this is not a kldload host", silently. Measured on .120 (2026-08-22): nine
// clones cloned out from the GUI, every one of them absent from state.db and
// therefore invisible to the Ansible inventory and the estate's reconcile
// annotation — while the exact same binary resolved fine from a root shell,
// which is why it had looked correct every time it was checked by hand.
//
// Returns an absolute path so callers can exec it without depending on the
// PATH of whatever session happens to be running.
func kldloadDB() string {
	// The locations kldload actually installs to, most specific first. sbin
	// before bin: that is where it lives, and checking it first keeps the
	// common case to one stat.
	return findTool("kldload-db",
		"/usr/local/sbin/kldload-db",
		"/usr/local/bin/kldload-db",
		"/usr/sbin/kldload-db",
		"/usr/bin/kldload-db",
	)
}

// findTool resolves an executable by PATH, then by the absolute fallbacks
// given, and returns "" when none of them is a runnable file.
//
// Split out from kldloadDB so the fallback behaviour is testable without
// depending on what happens to be installed on the machine running the tests.
func findTool(name string, fallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range fallbacks {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}
