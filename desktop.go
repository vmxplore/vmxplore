// desktop.go — turn a headless cloud image into a desktop, on first boot.
//
// What it does, in order:
//  1. Maps (distro, desktop) to that distro's real package group.
//  2. Emits the install command plus whatever else that distro needs to
//     actually reach a login screen — a display manager, a default target.
//  3. Refuses any pair it has not been taught, rather than guessing.
//
// WHY this is a recipe table and not a clever abstraction: every one of
// these names was WRONG when guessed from memory and had to be read off the
// real repositories. Fedora's GNOME is `workstation-product`, not
// `@workstation-product-environment`. Arch's are pacman GROUPS, invisible to
// `pacman -Si`. openSUSE ships patterns as ordinary packages. There is no
// rule connecting them; there is only what each vendor actually publishes.
//
// Every entry below was verified against the distribution's own repository
// in a container on 2026-08-10. Re-verify before adding a row — a dropdown
// that offers a combination the repo cannot satisfy fails ten minutes into a
// build, with the operator watching, and looks like a broken product.
//
// Inputs:  the spec's Distro and Desktop.
// Outputs: bash for the cloud-init post-installer, or "" for none.
//
// Notes: a desktop is 1.5–3 GB of packages. That turns a 90-second VM into a
// five-to-ten minute one, so callers MUST tell the operator — silence during
// a long first boot reads as a hang, which is the same lesson the appliance
// builds learned.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Desktops are the choices offered, in menu order. Empty means headless,
// which stays the default: a cloud image is a server unless asked otherwise.
func Desktops() []string { return []string{"none", "gnome", "kde", "xfce"} }

// recipe is how one distribution installs one desktop.
type recipe struct {
	// Pkgs is the install command's arguments — a group, a pattern or a
	// task package, whatever that distro actually uses.
	Pkgs []string
	// Extra is plain PACKAGES, never group ids. Split from Pkgs because
	// Fedora installs groups with `dnf group install`, which rejects a
	// package name outright — and rejects it by failing the whole
	// transaction, so one stray package means nothing at all is installed.
	// Distros whose install command takes both can have these appended.
	Extra []string
	// DM is the display-manager unit to enable. Named explicitly wherever
	// it is known, because "the group pulls one" is an assumption that was
	// FALSE for Fedora's kde-desktop — it pulls none at all.
	DM string
}

// desktopRecipes[distro][desktop]. Verified against real repositories,
// 2026-08-10 — see the file banner before editing.
var desktopRecipes = map[string]map[string]recipe{
	"fedora": {
		// ENVIRONMENT groups, not the bare product/desktop groups.
		// workstation-product is "Fedora Workstation product CORE" — a
		// GNOME shell and nothing else. A guest built from it came up with
		// no terminal, no file manager, no browser (2026-08-11). The
		// environment ids are what a Workstation install actually uses:
		//
		//   workstation-product-environment  1708 pkgs, gdm,     ptyxis
		//   kde-desktop-environment          2025 pkgs, NO DM,   konsole
		//   xfce-desktop-environment         1532 pkgs, lightdm, xfce4-terminal
		//
		// Counted in a container rather than guessed. KDE still ships no
		// display manager even as an environment, so it names sddm itself
		// or it installs 2000 packages with no way to log in. Every DM is
		// enabled explicitly: a no-op when the group already did it, and
		// insurance against a future group change removing the login screen.
		"gnome": {Pkgs: []string{"workstation-product-environment"}, DM: "gdm"},
		// sddm is in Extra, NOT Pkgs: `dnf group install kde-desktop-environment
		// sddm` fails with "No match for argument: sddm" and installs NOTHING,
		// leaving the guest with only the cloud image's base packages, no
		// desktop and no login screen (2026-08-11).
		"kde": {
			Pkgs:  []string{"kde-desktop-environment"},
			Extra: []string{"sddm"},
			DM:    "sddm",
		},
		"xfce": {Pkgs: []string{"xfce-desktop-environment"}, DM: "lightdm"},
	},
	// EL 10 (CentOS Stream, Rocky, AlmaLinux) — GNOME ONLY, and that is not
	// an oversight. Verified in quay.io/centos/centos:stream10, 2026-08-15:
	//
	//   workstation-product-environment  1109 pkgs, 1.3 GB, INCLUDES gdm
	//   kde-desktop-environment          "Error: Nothing to do"
	//   xfce-desktop-environment         "Error: Nothing to do"
	//
	// The KDE and XFCE group IDS EXIST -- `dnf group info` succeeds for both,
	// which is exactly the false positive this file's banner warns about. They
	// resolve to nothing installable without EPEL, so offering them would put
	// two entries in the dropdown that fail ten minutes into a build with the
	// operator watching. If EPEL is ever enabled by default on these guests,
	// re-verify and add them then.
	//
	// The environment install also logs "No match for group package
	// redhat-release" on CentOS Stream. That is harmless -- those are RHEL-only
	// packages named by the shared group definition -- and the transaction
	// resolves regardless.
	//
	// No cloud-kernel problem here, unlike Debian: EL cloud images run the
	// standard kernel, so DRM is present and the desktop actually draws.
	"centos": {
		"gnome": {Pkgs: []string{"workstation-product-environment"}, DM: "gdm"},
	},
	"rocky": {
		"gnome": {Pkgs: []string{"workstation-product-environment"}, DM: "gdm"},
	},
	"alma": {
		"gnome": {Pkgs: []string{"workstation-product-environment"}, DM: "gdm"},
	},
	// NOT amazon: Amazon Linux 2023 publishes no desktop group of any kind --
	// its groups are Container / Base / AMI / OnPrem variants only. gnome-shell
	// 47.3 exists as a package, so a desktop could be assembled by naming
	// individual packages, but that would be a recipe invented here rather than
	// one read off the vendor, which is the thing this file exists to avoid.
	"debian": {
		"gnome": {Pkgs: []string{"task-gnome-desktop"}},
		"kde":   {Pkgs: []string{"task-kde-desktop"}},
		"xfce":  {Pkgs: []string{"task-xfce-desktop"}},
	},
	"ubuntu": {
		// -minimal, not the full ubuntu-desktop: the extras are ~1GB of
		// office and games nobody asked a hypervisor for.
		"gnome": {Pkgs: []string{"ubuntu-desktop-minimal"}},
		"kde":   {Pkgs: []string{"kubuntu-desktop"}},
		"xfce":  {Pkgs: []string{"xubuntu-desktop"}},
	},
	"opensuse": {
		// Patterns are ordinary packages here; `-t pattern` is not needed
		// and the plain name resolves.
		"gnome": {Pkgs: []string{"patterns-gnome-gnome"}},
		"kde":   {Pkgs: []string{"patterns-kde-kde"}},
		"xfce":  {Pkgs: []string{"patterns-xfce-xfce"}},
	},
	"arch": {
		// pacman GROUPS, not packages — invisible to `pacman -Si`, which is
		// what made them look absent on the first check. Arch is also the
		// one distro whose desktop groups do not pull a display manager, so
		// each names its own.
		"gnome": {Pkgs: []string{"gnome"}, DM: "gdm"},
		"kde":   {Pkgs: []string{"plasma"}, DM: "sddm"},
		"xfce":  {Pkgs: []string{"xfce4", "lightdm", "lightdm-gtk-greeter"}, DM: "lightdm"},
	},
}

// DesktopsFor lists the desktops available for a distro, "none" first.
// A distro with no verified recipes offers only "none" — which is how the
// UI avoids promising something the repository cannot deliver.
func DesktopsFor(distro string) []string {
	out := []string{"none"}
	var named []string
	for d := range desktopRecipes[distro] {
		named = append(named, d)
	}
	sort.Strings(named)
	return append(out, named...)
}

// DesktopSupported reports whether this pair has a verified recipe.
func DesktopSupported(distro, desktop string) bool {
	if desktop == "" || desktop == "none" {
		return true
	}
	_, ok := desktopRecipes[distro][desktop]
	return ok
}

// desktopPostInstall returns the bash that installs a desktop on first boot.
//
// Args:    distro, desktop  as chosen in the dialog
// Returns: the script, or "" when headless or unknown.
// Failure modes callers must handle: an unknown pair returns "" rather than
// a guess. Validate with DesktopSupported first if you want to tell the
// operator why nothing happened.
func desktopPostInstall(distro, desktop string) string {
	if desktop == "" || desktop == "none" {
		return ""
	}
	r, ok := desktopRecipes[distro][desktop]
	if !ok {
		return ""
	}

	var install string
	switch distro {
	case "fedora":
		// `dnf group install <id>` handles environment ids too. An earlier
		// check used `install "@id"`, which dnf5 rejects — and the false
		// negative is what sent this to the bare -product groups and shipped
		// a desktop with no applications.
		install = "dnf group install -y " + strings.Join(r.Pkgs, " ")
		if len(r.Extra) > 0 {
			// A second command, because the first will not take a package
			// name. Chained so a failed group still fails the whole step.
			install += " && dnf install -y " + strings.Join(r.Extra, " ")
		}
	case "debian", "ubuntu":
		// apt-get update FIRST. A cloud image ships with NO package lists at
		// all -- /var/lib/apt/lists is empty on first boot -- so apt cannot
		// resolve anything and the install dies on "E: Unable to locate
		// package task-gnome-desktop" for a package that plainly exists.
		//
		// This is why the dnf distros worked and the apt ones did not:
		// `dnf group install` fetches its own metadata, apt does not. Verified
		// on VM 66 (Debian 13 trixie), 2026-08-15 -- zero Packages files
		// before, task-gnome-desktop resolving at 3.81 immediately after.
		//
		// Chained with && so a failed update fails the step rather than
		// running an install that cannot possibly succeed.
		//
		// noninteractive or tasksel stops to ask about keyboard layout and
		// waits forever on a machine with nobody at the console.
		// A DESKTOP KERNEL, not the cloud one.
		//
		// Debian's genericcloud image (and Ubuntu's kvm flavour) run a kernel
		// built for headless virtual machines: it ships NO GPU drivers at all.
		// There is no drivers/gpu directory in it and no virtio_gpu module, so
		// /dev/dri never appears, Xorg has no device, and gdm gives up with
		// "maximum number of X display failures reached" -- a fully installed
		// GNOME behind a permanently black screen.
		//
		// Measured on VM 1111 (Debian 13 trixie), 2026-08-15: running
		// 6.12.101+deb13-cloud-amd64, /dev/dri empty, no drm modules loaded,
		// 1598 packages of GNOME present and unusable. The generic kernel of
		// the SAME version ships drivers/gpu/drm/virtio.
		//
		// This is exactly why the dnf guests worked: Fedora's cloud image uses
		// its standard kernel, which has DRM.
		//
		// Installed in the same transaction as the desktop, so the reboot
		// cloud-init performs afterwards lands on it (grub takes the newest).
		// The alternative is to fetch the `generic` image instead of
		// `genericcloud` for desktop builds; this way works whatever image the
		// operator points at, including their own.
		kern := "linux-image-amd64"
		if distro == "ubuntu" {
			kern = "linux-image-generic"
		}
		install = "DEBIAN_FRONTEND=noninteractive apt-get update && " +
			"DEBIAN_FRONTEND=noninteractive apt-get install -y " + kern + " " +
			strings.Join(append(append([]string{}, r.Pkgs...), r.Extra...), " ")
	case "opensuse":
		install = "zypper --non-interactive install " +
			strings.Join(append(append([]string{}, r.Pkgs...), r.Extra...), " ")
	case "arch":
		install = "pacman -Sy --noconfirm " +
			strings.Join(append(append([]string{}, r.Pkgs...), r.Extra...), " ")
	default:
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Desktop: %s on %s (vmxplore). This is 1.5-3GB — first\n", desktop, distro)
	b.WriteString("# boot takes minutes, not seconds. Progress is on the console.\n")

	// ── The finisher unit, written BEFORE the long install ──────────────────
	//
	// HISTORY: 2026-08-15, VM "fed" (Fedora 44 KDE) on fiend. The desktop
	// installed completely and the guest still booted to a text login. The
	// cause was one line:
	//
	//     systemctl enable sddm
	//     → Failed to enable unit: File '/etc/systemd/system/display-manager.service'
	//       already exists and is a symlink to /usr/lib/systemd/system/plasmalogin.service
	//
	// Fedora 44's kde-desktop-environment ships PLASMALOGIN, not sddm, and
	// enables it itself. Enabling a second display manager is an error, and the
	// script ran under `set -Eeuo pipefail`, so it died on that line — before
	// `set-default graphical.target` and before its own success echo. Evidence
	// matched exactly: 1878 packages installed, no "installed" echo, no
	// "FAILED" echo either (it was inside the then-branch), default target
	// still multi-user.
	//
	// TWO lessons, both encoded below:
	//
	//  1. DO NOT NAME THE DISPLAY MANAGER when the distro already chose one.
	//     r.DM is a FALLBACK for distros whose desktop group enables nothing
	//     (Arch, and Fedora KDE as we believed until today). If
	//     display-manager.service already exists, that decision has been made
	//     by the packages and is more current than this table.
	//
	//  2. NOTHING HERE MAY ABORT THE SCRIPT. Every step is independently
	//     tolerant, because the steps after it are the ones that matter: a
	//     failed `enable` must never cost the `set-default` that follows.
	//
	// Written and enabled BEFORE the slow install so a reboot at any point —
	// including cloud-init's own power_state — costs a reboot, not a desktop.
	b.WriteString("cat >/var/lib/vmxplore-desktop-finish.sh <<'VMXFIN'\n")
	b.WriteString("#!/usr/bin/env bash\n")
	// NOT set -e. See lesson 2 above: this script exists precisely because a
	// set -e abort on an idempotent no-op cost a desktop.
	b.WriteString("set -uo pipefail\n")
	b.WriteString("# Whatever the desktop packages already enabled wins. Only pick a\n")
	b.WriteString("# display manager when the distro left that decision to us.\n")
	b.WriteString("if [ -e /etc/systemd/system/display-manager.service ]; then\n")
	b.WriteString("  dm=$(basename \"$(readlink -f /etc/systemd/system/display-manager.service)\")\n")
	b.WriteString("  echo \"vmxplore: display manager already enabled: ${dm}\"\n")
	b.WriteString("else\n")
	if r.DM != "" {
		fmt.Fprintf(&b, "  dm=%s.service\n", r.DM)
		fmt.Fprintf(&b, "  systemctl enable %s || echo 'vmxplore: could not enable %s' >&2\n", r.DM, r.DM)
	} else {
		// No group-provided DM and no fallback in the table: say so loudly
		// rather than leaving a silent text console to be discovered later.
		b.WriteString("  dm=\"\"\n")
		b.WriteString("  echo 'vmxplore: no display manager was enabled by the desktop packages " +
			"and none is configured — the desktop will not start' >&2\n")
	}
	b.WriteString("fi\n")
	b.WriteString("systemctl set-default graphical.target || " +
		"echo 'vmxplore: could not set graphical.target' >&2\n")
	// Bring the login screen up now too, so a guest that is ALREADY running
	// does not need a second reboot. `start`, not `isolate`: isolating would
	// tear down the transaction this unit is running inside.
	b.WriteString("[ -n \"$dm\" ] && systemctl start \"$dm\" || true\n")
	b.WriteString("systemctl disable vmxplore-desktop-finish.service || true\n")
	b.WriteString("VMXFIN\n")
	b.WriteString("chmod 0755 /var/lib/vmxplore-desktop-finish.sh\n")
	b.WriteString("cat >/etc/systemd/system/vmxplore-desktop-finish.service <<'VMXUNIT'\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=vmxplore: finish desktop setup (enable the display manager)\n")
	b.WriteString("# The install may still have been running when this was enabled, so wait\n")
	b.WriteString("# for a fully booted system rather than racing it a second time.\n")
	b.WriteString("After=multi-user.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=oneshot\n")
	b.WriteString("RemainAfterExit=yes\n")
	b.WriteString("ExecStart=/var/lib/vmxplore-desktop-finish.sh\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	b.WriteString("VMXUNIT\n")
	b.WriteString("systemctl daemon-reload\n")
	b.WriteString("systemctl enable vmxplore-desktop-finish.service\n")

	fmt.Fprintf(&b, "echo 'vmxplore: installing %s — this takes several minutes'\n", desktop)
	fmt.Fprintf(&b, "if %s; then\n", install)
	// The finisher will do this on the next boot regardless; running it here
	// too means a guest that is NOT rebooted still reaches a desktop.
	b.WriteString("  /var/lib/vmxplore-desktop-finish.sh || true\n")
	fmt.Fprintf(&b, "  echo 'vmxplore: %s installed — rebooting into the login screen'\n", desktop)
	b.WriteString("else\n")
	fmt.Fprintf(&b, "  echo 'vmxplore: %s install FAILED — the VM is still a working server' >&2\n", desktop)
	// Leave nothing armed that would flip a server to graphical.target after a
	// failed desktop install.
	b.WriteString("  systemctl disable vmxplore-desktop-finish.service || true\n")
	b.WriteString("fi\n")
	return b.String()
}
