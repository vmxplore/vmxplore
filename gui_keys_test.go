//go:build gui

// gui_keys_test.go — the fullscreen chord has to be one the toolkit will
// actually hand us.
//
// WHY these tests exist: a chord that Fyne never delivers does not fail
// visibly. It falls through to the console's keyboard forwarding and lands in
// the guest, or Fyne rewrites it into a built-in editing shortcut and does
// something else entirely. Either way the operator presses the documented key
// and the app appears to ignore it, with nothing in any log.
//
// HISTORY: 2026-08-11 — shift+insert shipped as the default and was Fyne's
// hardcoded Paste, so the fullscreen key typed the host clipboard into the
// guest; shift+f12 before it produced no shortcut at all and reached the guest
// as a plain keypress, opening Chrome's debugger inside the VM. Three builds
// went out with a fullscreen mode nobody could trigger.
package main

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

// The shipped default must survive its own validator. If this fails the
// binary starts with a fullscreen key that cannot work, which is the exact
// regression the validator was written for.
func TestDefaultChordIsUsable(t *testing.T) {
	sc, err := parseChord(defaultFullScreenChord)
	if err != nil {
		t.Fatalf("built-in default %q does not parse: %v",
			defaultFullScreenChord, err)
	}
	if err := chordDeliverable(sc); err != nil {
		t.Fatalf("built-in default %q is not deliverable: %v",
			defaultFullScreenChord, err)
	}
}

// resolveFullScreenKey must never hand back a nil shortcut: both call sites
// dereference it on every keypress.
func TestResolveNeverReturnsNil(t *testing.T) {
	for _, env := range []string{"", "shift+insert", "total nonsense", "ctrl+c"} {
		t.Setenv("VMX_FULLSCREEN_KEY", env)
		sc, label := resolveFullScreenKey()
		if sc == nil {
			t.Fatalf("VMX_FULLSCREEN_KEY=%q resolved to a nil shortcut", env)
		}
		if label == "" {
			t.Fatalf("VMX_FULLSCREEN_KEY=%q resolved to an empty label", env)
		}
	}
}

// The chords Fyne cannot deliver, each rejected at parse time rather than at
// the moment the operator needs the key.
func TestUndeliverableChordsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		chord, why string
	}{
		{"shift+insert", "Fyne hardcodes this to Paste"},
		{"shift+f12", "Shift-only never becomes a CustomShortcut"},
		{"shift+delete", "Fyne hardcodes this to Cut"},
		{"shift+a", "Shift-only never becomes a CustomShortcut"},
		{"ctrl+c", "Fyne's built-in Copy"},
		{"ctrl+v", "Fyne's built-in Paste"},
		{"ctrl+a", "Fyne's built-in SelectAll"},
		{"ctrl+insert", "Fyne's built-in Copy (alternative)"},
	} {
		if _, err := parseChord(tc.chord); err == nil {
			t.Errorf("parseChord(%q) accepted a chord that can never fire (%s)",
				tc.chord, tc.why)
		}
	}
}

// Adding a second modifier takes a chord back out of Fyne's reserved set, so
// the validator must not over-reject: ctrl+shift+c is ours, ctrl+c is not.
func TestDeliverableChordsAreAccepted(t *testing.T) {
	for _, chord := range []string{
		"alt+insert", "alt+delete", "ctrl+shift+c", "ctrl+f12",
		"super+return", "ctrl+alt+f", "alt+f11",
	} {
		if _, err := parseChord(chord); err != nil {
			t.Errorf("parseChord(%q) rejected a usable chord: %v", chord, err)
		}
	}
}

// The label the console header prints and the shortcut the handlers compare
// against come from one call, so they cannot drift apart. Guard that they
// describe the same key.
func TestLabelMatchesTheBinding(t *testing.T) {
	t.Setenv("VMX_FULLSCREEN_KEY", "ctrl+alt+f")
	sc, label := resolveFullScreenKey()
	if label != "ctrl+alt+f" {
		t.Fatalf("label = %q, want the chord that was set", label)
	}
	want := &desktop.CustomShortcut{
		KeyName:  fyne.KeyName("F"),
		Modifier: fyne.KeyModifierControl | fyne.KeyModifierAlt,
	}
	if sc.KeyName != want.KeyName || sc.Modifier != want.Modifier {
		t.Fatalf("binding = %v/%v, want %v/%v",
			sc.KeyName, sc.Modifier, want.KeyName, want.Modifier)
	}
}

// The window half of the toggle is platform-conditional, and the override has
// to work in both directions: an operator on a single-monitor Wayland desktop
// wants it on (no wrong head exists), and one who dislikes it wants it off
// everywhere. Defaults are asserted per session type.
func TestDriveWindowFullScreen(t *testing.T) {
	for _, tc := range []struct {
		name, sessionType, waylandDisplay, override string
		want                                        bool
	}{
		{"x11 drives the window", "x11", "", "", true},
		{"wayland does not", "wayland", "", "", false},
		{"WAYLAND_DISPLAY alone is enough", "", "wayland-0", "", false},
		{"always overrides wayland", "wayland", "wayland-0", "always", true},
		{"never overrides x11", "x11", "", "never", false},
		{"junk override falls back to the default", "wayland", "", "maybe", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_SESSION_TYPE", tc.sessionType)
			t.Setenv("WAYLAND_DISPLAY", tc.waylandDisplay)
			t.Setenv("VMX_FULLSCREEN_WINDOW", tc.override)
			if got := driveWindowFullScreen(); got != tc.want {
				t.Errorf("driveWindowFullScreen() = %v, want %v", got, tc.want)
			}
		})
	}
}
