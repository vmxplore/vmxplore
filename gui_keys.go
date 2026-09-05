//go:build gui

// gui_keys.go — the one keyboard chord the console keeps for itself.
//
// What it does, in order:
//  1. reads VMX_FULLSCREEN_KEY, falling back to the default chord;
//  2. parses it into the Fyne shortcut the GUI registers and the console
//     widget compares against;
//  3. on anything unparseable, warns once and uses the default — a typo in
//     an environment variable must not cost you the only way out of a
//     fullscreen console.
//
// WHY this is configurable at all, when nothing else in the tool is:
// every key belongs to the guest. This console forwards the entire keyboard
// — that is the job — so any chord bound here is one the guest can NEVER
// receive, with no escape sequence to get it back. Picking a universal
// default is not possible: F12 is the firmware boot menu and kldload's own
// sysdiag, F11 is fullscreen in every browser (including the Firefox kiosk
// in our own Apps catalog), Shift+F12 is a debugger binding, and Alt+Delete
// lives one modifier away from Ctrl+Alt+Delete. Whatever ships as the
// default will be wrong for somebody's guest, so the answer is that they can
// change it without waiting for a release.
//
// The shipped default, Shift+Insert, is not innocent either: it is paste from
// the X11 primary selection in every Linux terminal, and since Ctrl+V is also
// intercepted a guest is left with neither paste key. It was chosen anyway —
// pasting INTO a guest is what Ctrl+V here already does, and this is the
// chord an operator can most easily remember. Set VMX_FULLSCREEN_KEY if that
// trade is wrong for the guests you run.
//
// Notes: the default is deliberately NOT Ctrl+Alt+anything — Ctrl+Alt+Fn is
// virtual-terminal switching in Linux guests, and Ctrl+Alt+Delete is the one
// chord an operator most expects a console to pass through untouched.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

// defaultFullScreenChord is what ships. See the banner for why no value here
// can be right for every guest, and chordDeliverable for why it may not be a
// Shift chord however appealing one looks.
const defaultFullScreenChord = "alt+insert"

// namedKeys are the non-alphanumeric keys a chord may name. Letters and
// digits are resolved directly (Fyne names them "A".."Z" and "0".."9"), so
// only the ones with spelled-out names need a table.
var namedKeys = map[string]fyne.KeyName{
	"delete": fyne.KeyDelete, "del": fyne.KeyDelete,
	"insert": fyne.KeyInsert, "ins": fyne.KeyInsert,
	"home": fyne.KeyHome, "end": fyne.KeyEnd,
	"pageup": fyne.KeyPageUp, "pagedown": fyne.KeyPageDown,
	"space": fyne.KeySpace, "return": fyne.KeyReturn, "enter": fyne.KeyEnter,
	"backspace": fyne.KeyBackspace, "tab": fyne.KeyTab,
	"f1": fyne.KeyF1, "f2": fyne.KeyF2, "f3": fyne.KeyF3, "f4": fyne.KeyF4,
	"f5": fyne.KeyF5, "f6": fyne.KeyF6, "f7": fyne.KeyF7, "f8": fyne.KeyF8,
	"f9": fyne.KeyF9, "f10": fyne.KeyF10, "f11": fyne.KeyF11, "f12": fyne.KeyF12,
}

var namedModifiers = map[string]fyne.KeyModifier{
	"shift": fyne.KeyModifierShift,
	"ctrl":  fyne.KeyModifierControl, "control": fyne.KeyModifierControl,
	"alt":   fyne.KeyModifierAlt,
	"super": fyne.KeyModifierSuper, "meta": fyne.KeyModifierSuper,
	"win": fyne.KeyModifierSuper,
}

// parseChord turns "alt+delete" or "shift+f11" into a Fyne shortcut.
//
// Args:    s — modifier names and one key, joined by '+', case-insensitive.
// Returns: the shortcut, or an error naming the part that did not parse.
//
// At least one modifier is required. A bare key would be swallowed from the
// guest with no way to type it at all, which for a console is a bug however
// carefully the operator asked for it.
func parseChord(s string) (*desktop.CustomShortcut, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "+")
	if len(parts) < 2 {
		return nil, fmt.Errorf("%q needs at least one modifier and a key, "+
			"like alt+delete — a bare key would be taken from the guest "+
			"with no way to send it", s)
	}
	var mod fyne.KeyModifier
	for _, p := range parts[:len(parts)-1] {
		m, ok := namedModifiers[strings.TrimSpace(p)]
		if !ok {
			return nil, fmt.Errorf("unknown modifier %q in %q", p, s)
		}
		mod |= m
	}
	name := strings.TrimSpace(parts[len(parts)-1])
	key, ok := namedKeys[name]
	if !ok {
		// Letters and digits: Fyne's KeyName for these is the uppercase
		// character itself, so a single-rune part resolves directly.
		if len([]rune(name)) == 1 {
			key = fyne.KeyName(strings.ToUpper(name))
		} else {
			return nil, fmt.Errorf("unknown key %q in %q", name, s)
		}
	}
	sc := &desktop.CustomShortcut{KeyName: key, Modifier: mod}
	// Parsing is not enough: a chord can be perfectly well-formed and still
	// be one the toolkit will never hand us. Reject those HERE, where the
	// operator finds out at startup, rather than at the moment they need it.
	if err := chordDeliverable(sc); err != nil {
		return nil, fmt.Errorf("%q %w", s, err)
	}
	return sc, nil
}

// driveWindowFullScreen reports whether the fullscreen toggle should also put
// the window itself fullscreen, or only strip the panes and leave the frame
// to the compositor.
//
// Returns: true to call SetFullScreen from the toggle. The default is true
// everywhere; VMX_FULLSCREEN_WINDOW=never turns the window half off.
//
// HISTORY: this was false under Wayland from 2026-08-11 to 2026-09-04. Fyne
// picks the target monitor itself, and under Wayland it cannot: the
// protocol never tells a client where its own window is, the geometry
// search in getMonitorForWindow sits behind `if !build.IsWayland`, and
// what remains is GLFW's "primary" monitor — the first output announced.
// On a multi-head GNOME desktop the console jumped to another screen on
// every toggle (window on monitor 1, fullscreen on monitor 3). The way out
// was the compositor's own fullscreen key, which always uses the head the
// window is on.
//
// The protocol allows exactly that from the client side too:
// xdg_toplevel.set_fullscreen with a NULL output lets the compositor pick
// the current one. GLFW never passes NULL and nothing above it can ask, so
// the build carries a patched copy of GLFW that does
// (third_party/glfw/VMXPLORE-PATCH.md, gated by TestGLFWPatchPresent).
// With that in place the one chord does both halves on every platform, as
// the operator asked on 2026-09-04.
func driveWindowFullScreen() bool {
	switch strings.ToLower(strings.TrimSpace(
		os.Getenv("VMX_FULLSCREEN_WINDOW"))) {
	case "always", "1", "yes", "true":
		return true
	case "never", "0", "no", "false":
		return false
	}
	return true
}

// ctrlShortcutModifier is the modifier Fyne reserves for the standard editing
// chords: Super on macOS, Control everywhere else. Mirrors ctrlMod in the glfw
// driver (internal/driver/glfw/window.go), which picks it by GOOS.
func ctrlShortcutModifier() fyne.KeyModifier {
	if runtime.GOOS == "darwin" {
		return fyne.KeyModifierSuper
	}
	return fyne.KeyModifierControl
}

// ctrlReserved are the keys Fyne converts into built-in editing shortcuts when
// the ONLY modifier held is ctrlShortcutModifier(). They never arrive as a
// desktop.CustomShortcut, so binding one here is binding nothing.
var ctrlReserved = map[fyne.KeyName]string{
	fyne.KeyZ: "Undo", fyne.KeyY: "Redo", fyne.KeyV: "Paste",
	fyne.KeyC: "Copy", fyne.KeyX: "Cut", fyne.KeyA: "SelectAll",
	fyne.KeyInsert: "Copy",
}

// chordDeliverable reports whether Fyne will ever deliver this chord to us as
// a custom shortcut.
//
// Args:    sc — a parsed chord.
// Returns: nil if the chord is bindable; otherwise an error phrased for an
//
//	operator who set VMX_FULLSCREEN_KEY and needs to pick again.
//
// WHY this exists at all: a chord that cannot be delivered does not fail
// safely. It falls through to the console's normal keyboard forwarding and
// lands in the GUEST, or — worse — Fyne rewrites it into a built-in editing
// shortcut and something entirely different happens.
//
// HISTORY: 2026-08-11. The shipped default was shift+insert, and both failure
// modes were live at once. Read triggerKey in fyne internal/driver/glfw/
// window.go: Shift+Insert is hardcoded to fyne.ShortcutPaste, so the default
// fullscreen key pasted the host clipboard INTO the guest; and the guard
// building CustomShortcut excludes `modifier == fyne.KeyModifierShift`
// outright, so shift+f12 (the previous default, still the name of the commit
// that introduced it) produced no shortcut at all and reached the guest as a
// plain keypress — which is why pressing it opened Chrome's debugger inside
// the VM. Nobody could trigger fullscreen for three builds. The console-only
// layout was working the whole time; only the key was dead.
func chordDeliverable(sc *desktop.CustomShortcut) error {
	if sc.Modifier == 0 {
		return fmt.Errorf("needs a modifier")
	}
	// The hard one: Fyne builds a CustomShortcut only when the modifier set
	// is something other than Shift alone. Shift+ANYTHING is unbindable, and
	// no amount of registering it changes that.
	if sc.Modifier == fyne.KeyModifierShift {
		return fmt.Errorf("uses Shift as its only modifier, which this " +
			"toolkit never delivers as a custom shortcut — the key would " +
			"go to the guest instead. Add ctrl, alt or super")
	}
	if sc.Modifier == ctrlShortcutModifier() {
		if what, taken := ctrlReserved[sc.KeyName]; taken {
			return fmt.Errorf("is this toolkit's built-in %s shortcut and "+
				"never arrives as a custom binding — add another modifier, "+
				"or pick a different key", what)
		}
	}
	return nil
}

// fullScreenKey is the resolved chord, read once at startup. Both the canvas
// registration and the console widget's own handler compare against this, so
// there is exactly one place the answer comes from.
var fullScreenKey, fullScreenKeyLabel = resolveFullScreenKey()

func resolveFullScreenKey() (*desktop.CustomShortcut, string) {
	want := os.Getenv("VMX_FULLSCREEN_KEY")
	if want == "" {
		want = defaultFullScreenChord
	}
	sc, err := parseChord(want)
	if err != nil {
		// Loud, once, and then carry on with a console that still works.
		fmt.Fprintf(os.Stderr, "vmxplore: VMX_FULLSCREEN_KEY: %v; "+
			"using %s\n", err, defaultFullScreenChord)
		sc, err = parseChord(defaultFullScreenChord)
		if err != nil {
			// The default failing its own validation is a build defect, not
			// an operator error — but this runs at package init, so returning
			// nil here would turn it into a nil dereference on the first
			// keypress instead of a message. TestDefaultChordIsUsable is what
			// actually keeps this branch unreachable.
			fmt.Fprintf(os.Stderr, "vmxplore: BUG: built-in default %q is "+
				"not a usable chord: %v\n", defaultFullScreenChord, err)
			return &desktop.CustomShortcut{
				KeyName:  fyne.KeyInsert,
				Modifier: fyne.KeyModifierAlt,
			}, "alt+insert"
		}
		return sc, defaultFullScreenChord
	}
	return sc, want
}
