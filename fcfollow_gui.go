//go:build gui

// fcfollow_gui.go — clone follow-up helpers that only the GUI calls
//
// These live behind the `gui` build tag because gui.go is their only caller.
// They used to sit in fcfollow.go, which carries no tag and so compiles into the
// static TUI flavour too; there they were genuinely dead code, and
// `staticcheck ./...` (the non-gui half of `make staticcheck`) failed the
// build on five U1000 "is unused" reports. Splitting them is the honest fix:
// the TUI binary no longer carries GUI-only helpers, and the gui build is
// unchanged.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// cloneFollowUpLabel is the checkbox text for a golden.
func cloneFollowUpLabel(golden string, port int, n int) string {
	switch cloneFollowUp(golden, port) {
	case "wall":
		return "When done, open the VDI wall — every desktop on one page"
	case "rdp":
		return fmt.Sprintf("When done, open an RDP session to each of the %d desktop(s)", n)
	case "browser":
		return fmt.Sprintf("When done, open each of the %d instance(s) in a browser tab", n)
	}
	return "Nothing to open when done — this golden serves no port"
}

// rdpClientArgv is the local RDP client to launch for one address: Remmina
// when present (it prompts for the login, which is the tile's guest
// account), else FreeRDP without a password on the command line — a
// credential in argv is a credential in `ps`. Empty when neither exists.
func rdpClientArgv(ip string) []string {
	if _, err := exec.LookPath("remmina"); err == nil {
		return []string{"remmina", "-c", "rdp://" + ip}
	}
	for _, c := range []string{"xfreerdp3", "xfreerdp"} {
		if _, err := exec.LookPath(c); err == nil {
			return []string{c, "/v:" + ip, "/cert:ignore", "/dynamic-resolution"}
		}
	}
	return nil
}

// remminaPrefPath is where Remmina keeps its preferences for this user.
func remminaPrefPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "remmina", "remmina.pref")
}

// remminaRunning reports whether a Remmina instance is already up — the one
// that would swallow new connections as tabs under its old setting.
func remminaRunning() bool {
	return exec.Command("pgrep", "-x", "remmina").Run() == nil
}
