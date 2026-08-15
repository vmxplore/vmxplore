package main

import (
	"strings"
	"testing"
)

// The desktop must be armed BEFORE the slow install, not after it.
//
// HISTORY: 2026-08-15, VM "fed" on fiend. `systemctl enable sddm` and
// `set-default graphical.target` were the last two lines of the post-install
// script, and cloud-init's power_state reboot fired two seconds after the
// final dnf returned. Both installs succeeded; neither systemctl line ran; the
// guest booted to a text login with a complete KDE and sddm disabled.
//
// This test fails if anyone moves the enabling back behind the install.
func TestDesktopFinisherIsArmedBeforeTheInstall(t *testing.T) {
	for _, tc := range []struct{ distro, desktop, installNeedle string }{
		{"fedora", "kde", "dnf group install"},
		{"fedora", "gnome", "dnf group install"},
		{"debian", "kde", "apt-get install"},
		{"ubuntu", "gnome", "apt-get install"},
		{"arch", "kde", "pacman -Sy"},
	} {
		script := desktopPostInstall(tc.distro, tc.desktop)
		if script == "" {
			t.Fatalf("%s/%s produced no script", tc.distro, tc.desktop)
		}

		armed := strings.Index(script, "systemctl enable vmxplore-desktop-finish.service")
		install := strings.Index(script, tc.installNeedle)
		if armed < 0 {
			t.Errorf("%s/%s: finisher unit is never enabled", tc.distro, tc.desktop)
			continue
		}
		if install < 0 {
			t.Errorf("%s/%s: install command %q missing", tc.distro, tc.desktop, tc.installNeedle)
			continue
		}
		if armed > install {
			t.Errorf("%s/%s: finisher is armed AFTER the install (at %d vs %d) — "+
				"a reboot during the install loses the desktop again",
				tc.distro, tc.desktop, armed, install)
		}
	}
}

// The finisher must set the default target, and for any distro whose recipe
// names a display manager it must enable that DM. A desktop you cannot log
// into is the failure this whole path exists to prevent.
func TestDesktopFinisherEnablesTheDisplayManager(t *testing.T) {
	script := desktopPostInstall("fedora", "kde")
	for _, want := range []string{
		// The fallback is still named for distros that need one...
		"systemctl enable sddm",
		"systemctl set-default graphical.target",
		// ...but an already-enabled display manager must win. Fedora 44 KDE
		// ships plasmalogin, and enabling sddm on top of it is an ERROR that
		// used to abort the whole script under set -e.
		"if [ -e /etc/systemd/system/display-manager.service ]; then",
		"display manager already enabled",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("finisher is missing %q", want)
		}
	}
}

// A FAILED install must not leave a unit armed that flips a working server to
// graphical.target on its next boot.
func TestFailedInstallDisarmsTheFinisher(t *testing.T) {
	script := desktopPostInstall("fedora", "kde")
	// LastIndex, not Index: the finisher script disables itself with the very
	// same line, inside its heredoc, near the TOP of the generated output. A
	// first-match search finds that one and concludes the failure branch is
	// missing — which is what this test did on its first run.
	elseIdx := strings.LastIndex(script, "\nelse\n")
	disable := strings.LastIndex(script, "systemctl disable vmxplore-desktop-finish.service || true")
	if elseIdx < 0 {
		t.Fatal("no else branch in the generated script")
	}
	if disable < elseIdx {
		t.Error("the failure branch does not disarm the finisher unit — a failed " +
			"desktop install would still flip this server to graphical.target")
	}
}

// Headless VMs must be untouched: no unit, no graphical.target, nothing.
func TestHeadlessStaysHeadless(t *testing.T) {
	for _, d := range []string{"", "none"} {
		if s := desktopPostInstall("fedora", d); s != "" {
			t.Errorf("desktop %q produced a script: %q", d, s)
		}
	}
}
