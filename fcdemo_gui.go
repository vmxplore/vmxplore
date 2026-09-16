//go:build gui

// fcdemo_gui.go — demo-tile helpers that only the GUI calls
//
// These live behind the `gui` build tag because gui.go is their only caller.
// They used to sit in fcdemo.go, which carries no tag and so compiles into the
// static TUI flavour too; there they were genuinely dead code, and
// `staticcheck ./...` (the non-gui half of `make staticcheck`) failed the
// build on five U1000 "is unused" reports. Splitting them is the honest fix:
// the TUI binary no longer carries GUI-only helpers, and the gui build is
// unchanged.

package main

import (
	"os/exec"
	"strings"
)

// demoTeardownRunning reports whether a kfire destroy is in flight. pgrep is
// the probe because the teardown is a child process, not a state file.
//
// The pattern is ANCHORED at the start of the command line. A bare
// `pgrep -f "kfire destroy"` matches any process whose arguments merely
// contain that text — a shell one-liner about it, an editor with the script
// open, or the very test that was checking the probe (caught 2026-09-06).
// A false positive here blocks the demo for no reason, which is worse than
// the race it exists to prevent.
func demoTeardownRunning() bool {
	const pat = `^(sudo( +-[A-Za-z]+)* +)?[^ ]*kfire +(destroy|golden-destroy)\b`
	out, err := exec.Command("pgrep", "-f", pat).Output()
	if err != nil {
		// pgrep exits 1 when nothing matches, which is the common case and
		// not an error worth reporting.
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}
