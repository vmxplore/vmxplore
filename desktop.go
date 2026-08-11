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
	// DM is the display-manager unit to enable, when the package set does
	// not pull and enable one itself. Arch is the case that needs it.
	DM string
}

// desktopRecipes[distro][desktop]. Verified against real repositories,
// 2026-08-10 — see the file banner before editing.
var desktopRecipes = map[string]map[string]recipe{
	"fedora": {
		"gnome": {Pkgs: []string{"workstation-product"}},
		"kde":   {Pkgs: []string{"kde-desktop"}},
		"xfce":  {Pkgs: []string{"xfce-desktop"}},
	},
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
		install = "dnf group install -y " + strings.Join(r.Pkgs, " ")
	case "debian", "ubuntu":
		// noninteractive or tasksel stops to ask about keyboard layout and
		// waits forever on a machine with nobody at the console.
		install = "DEBIAN_FRONTEND=noninteractive apt-get install -y " +
			strings.Join(r.Pkgs, " ")
	case "opensuse":
		install = "zypper --non-interactive install " + strings.Join(r.Pkgs, " ")
	case "arch":
		install = "pacman -Sy --noconfirm " + strings.Join(r.Pkgs, " ")
	default:
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Desktop: %s on %s (vmxplore). This is 1.5-3GB — first\n", desktop, distro)
	b.WriteString("# boot takes minutes, not seconds. Progress is on the console.\n")
	fmt.Fprintf(&b, "echo 'vmxplore: installing %s — this takes several minutes'\n", desktop)
	fmt.Fprintf(&b, "if %s; then\n", install)
	if r.DM != "" {
		// Only Arch needs this; elsewhere the task/group enables its own.
		fmt.Fprintf(&b, "  systemctl enable %s\n", r.DM)
	}
	// Without this the packages are installed and the machine still boots to
	// a text console, which reads as "the desktop did not install".
	b.WriteString("  systemctl set-default graphical.target\n")
	fmt.Fprintf(&b, "  echo 'vmxplore: %s installed — reboot for the login screen'\n", desktop)
	b.WriteString("else\n")
	fmt.Fprintf(&b, "  echo 'vmxplore: %s install FAILED — the VM is still a working server' >&2\n", desktop)
	b.WriteString("fi\n")
	return b.String()
}
