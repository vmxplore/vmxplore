//go:build gui

// gui_term.go — `vmxplore --term [command ...]`: vmxplore's terminal as a
// window of its own, running one command (default `vmx`).
//
// Why: the operator likes vmxplore's terminal and vmx's interface, and wants
// one icon for each console (2026-09-30). The GUI's terminal only existed as
// a pane inside the estate window; this is the same widget (fyne-io/terminal)
// and the same pty plumbing as that pane, alone in a window, so:
//   - the TUI's desktop icon is `vmxplore --term vmx`;
//   - vmx, on a desktop, can open a guest's console in it
//     (`vmxplore --term ssh ...`, `vmxplore --term virsh console ...`).
//
// The window closes when the command exits, and the exit status is the
// command's, so a launcher or a script can tell how it ended.
//
// Notes:
//   - The widget only resizes a pty it started itself; given a pty by
//     RunWithConnection it never does, and a TUI then draws at the size it
//     was born with. A listener on the widget's Config resizes ours.
//   - Its own app ID (dev.vmxplore.vmx) and a FIXED title "vmx": the title
//     is what the desktop matches to an icon (WM_CLASS follows it, see
//     runGUI's banner), so the window keeps it rather than following the
//     titles programs set with escape codes, and groups under the TUI's
//     icon, not the estate window's.

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/theme"
	"github.com/creack/pty"
	fyneterm "github.com/fyne-io/terminal"
)

//go:embed packaging/vmx.svg
var iconTUISVG []byte

func runTermGUI(argv []string) int {
	if len(argv) == 0 {
		argv = []string{"vmx"}
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		fmt.Fprintf(os.Stderr, "vmxplore --term: %s: not found on PATH\n", argv[0])
		return 127
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	p, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmxplore --term: starting %s: %v\n", argv[0], err)
		return 1
	}

	a := app.NewWithID("dev.vmxplore.vmx")
	a.Settings().SetTheme(compactTheme{theme.DefaultTheme()})
	a.SetIcon(fyne.NewStaticResource("vmx.svg", iconTUISVG))
	w := a.NewWindow("vmx")
	term := fyneterm.New()
	term.AddShortcut(fullScreenKey, func(fyne.Shortcut) { w.SetFullScreen(!w.FullScreen()) })

	// Resize the pty whenever the widget's grid changes (see Notes).
	cfgs := make(chan fyneterm.Config, 4)
	term.AddListener(cfgs)
	go func() {
		for c := range cfgs {
			if c.Columns > 0 && c.Rows > 0 {
				_ = pty.Setsize(p, &pty.Winsize{Rows: uint16(c.Rows), Cols: uint16(c.Columns)})
			}
		}
	}()

	status := 0
	go func() {
		_ = term.RunWithConnection(p, p)
		if err := cmd.Wait(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				status = ee.ExitCode()
			} else {
				status = 1
			}
		}
		fyne.Do(a.Quit)
	}()

	w.SetContent(term)
	w.Resize(fyne.NewSize(1280, 800))
	w.Canvas().Focus(term)
	w.SetCloseIntercept(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = p.Close()
		a.Quit()
	})
	w.ShowAndRun()
	return status
}
