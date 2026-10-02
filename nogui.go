//go:build !gui

// nogui.go — the static build's stand-in for the GUI (the family pattern:
// zxplore/nogui.go is byte-for-byte the same idea).
//
// Built WITHOUT the `gui` tag (CGO_ENABLED=0), vmx is a single static
// TUI-only binary — no cgo, no OpenGL, no X/Wayland — that you can scp to
// any libvirt box. Asking that build for the GUI lands here.
package main

import (
	"fmt"
	"os"
)

// runConsoleGUI needs the GUI: the static build can only say so.
func runConsoleGUI(name string) int {
	fmt.Fprintf(os.Stderr, "vmx: --console %s needs the GUI build (make gui, or go build -tags gui)\n", name)
	return 2
}

// runGUI, in the static build (vmxctl), is "no command given": print the
// help and exit 2. It started the old built-in TUI, but the terminal console
// is vmx now (cmd/vmx) and vmxctl takes commands (operator, 2026-09-30).
// --tui still starts the built-in TUI explicitly.
func runGUI(rs *Ruleset) {
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}

// hasGUI: see gui_only.go — this is the build without the window.
const hasGUI = false

func runTermGUI(argv []string) int {
	fmt.Fprintln(os.Stderr, "vmx: --term needs the GUI build (make gui, or go build -tags gui)")
	return 2
}
