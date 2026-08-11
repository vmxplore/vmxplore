package main

import (
	"strings"
	"testing"
)

// Every pair here was read off the real repository. This test is the record
// of WHICH pairs were verified — adding a row without verifying it is the
// failure mode the whole table exists to prevent.
func TestVerifiedPairsAllProduceAnInstall(t *testing.T) {
	want := map[string][]string{
		"fedora":   {"workstation-product", "kde-desktop", "xfce-desktop"},
		"debian":   {"task-gnome-desktop", "task-kde-desktop", "task-xfce-desktop"},
		"ubuntu":   {"ubuntu-desktop-minimal", "kubuntu-desktop", "xubuntu-desktop"},
		"opensuse": {"patterns-gnome-gnome", "patterns-kde-kde", "patterns-xfce-xfce"},
		"arch":     {"gnome", "plasma", "xfce4"},
	}
	for distro, pkgs := range want {
		for i, desktop := range []string{"gnome", "kde", "xfce"} {
			out := desktopPostInstall(distro, desktop)
			if out == "" {
				t.Errorf("%s/%s produced nothing", distro, desktop)
				continue
			}
			if !strings.Contains(out, pkgs[i]) {
				t.Errorf("%s/%s should install %q:\n%s", distro, desktop, pkgs[i], out)
			}
			// Installing the packages and leaving multi-user.target reads as
			// "the desktop did not install".
			if !strings.Contains(out, "set-default graphical.target") {
				t.Errorf("%s/%s must switch to graphical.target", distro, desktop)
			}
		}
	}
}

// Headless stays the default: a cloud image is a server unless asked.
func TestHeadlessIsTheDefault(t *testing.T) {
	for _, d := range []string{"", "none"} {
		if got := desktopPostInstall("fedora", d); got != "" {
			t.Errorf("desktop %q should install nothing, got:\n%s", d, got)
		}
	}
}

// An unverified pair must produce nothing rather than a guess. Offering a
// combination the repo cannot satisfy fails ten minutes into a build.
func TestUnknownPairsAreRefused(t *testing.T) {
	for _, c := range [][2]string{
		{"rocky", "kde"},    // EL desktops not verified yet
		{"alma", "gnome"},   // ditto
		{"amazon", "gnome"}, // cloud-only image, no desktop story
		{"fedora", "mate"},  // desktop we never taught
	} {
		if got := desktopPostInstall(c[0], c[1]); got != "" {
			t.Errorf("%s/%s is unverified and must produce nothing:\n%s", c[0], c[1], got)
		}
		if DesktopSupported(c[0], c[1]) {
			t.Errorf("%s/%s must not report as supported", c[0], c[1])
		}
	}
}

// The menu must never offer a pair the table cannot satisfy.
func TestMenuOffersOnlyWhatWorks(t *testing.T) {
	for _, distro := range CloudDistros() {
		for _, d := range DesktopsFor(distro) {
			if !DesktopSupported(distro, d) {
				t.Errorf("menu offers %s/%s but it is not supported", distro, d)
			}
		}
	}
	// A distro with no recipes offers headless only.
	if got := DesktopsFor("amazon"); len(got) != 1 || got[0] != "none" {
		t.Errorf("amazon has no verified desktops, menu should be [none], got %v", got)
	}
}

// Debian and Ubuntu stop to ask about keyboard layout without this, and wait
// forever on a machine with nobody at the console.
func TestAptInstallsAreNoninteractive(t *testing.T) {
	for _, distro := range []string{"debian", "ubuntu"} {
		out := desktopPostInstall(distro, "gnome")
		if !strings.Contains(out, "DEBIAN_FRONTEND=noninteractive") {
			t.Errorf("%s must install noninteractively:\n%s", distro, out)
		}
	}
}

// Arch is the one whose groups do not pull a display manager.
func TestArchEnablesItsDisplayManager(t *testing.T) {
	for desktop, dm := range map[string]string{"gnome": "gdm", "kde": "sddm", "xfce": "lightdm"} {
		out := desktopPostInstall("arch", desktop)
		if !strings.Contains(out, "systemctl enable "+dm) {
			t.Errorf("arch/%s must enable %s:\n%s", desktop, dm, out)
		}
	}
}
