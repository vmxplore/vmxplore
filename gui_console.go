//go:build gui

// gui_console.go — `vmxplore --console VM`: one VM's screen, the whole window,
// and nothing else.
//
// What it does, in order:
//  1. finds the domain's VNC display the way the estate GUI does (vncPort,
//     vncEndpoint), and dials it;
//  2. opens one window holding only the console viewer, fullscreen, keyboard
//     focus on the guest;
//  3. quits on the quit chord (VMX_QUIT_KEY, Ctrl+Alt+Q unless set) or when
//     the display closes -- the guest powered off, the hypervisor went away.
//
// WHY: a lean KVM host has no desktop, and the operator still wants every
// VM's GUI "natively on just a shell". `cage -- vmxplore --console web1` from a
// bare tty does it: cage (already on every kldload install for the install
// kiosk) is a compositor for exactly one app, so nothing else is drawn and
// nothing stays running afterwards. Proven by hand on fiend, 2026-09-27:
// "I have a full gnome desktop launched from prompt". Under cage there is no
// window frame to close and no other way out, which is why the quit chord is
// not optional here.
//
// Exit: 0 after a quit or a closed display, 1 when the VM or its display
// cannot be reached (the reason is on stderr before any window opens).
package main

import (
	"fmt"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/driver/desktop"
)

const defaultQuitKey = "ctrl+alt+q"

// resolveQuitKey is VMX_QUIT_KEY parsed, or the default when it is unset or
// unusable -- warned, never fatal: a typo must not cost the only way out.
func resolveQuitKey() (*desktop.CustomShortcut, string) {
	want := os.Getenv("VMX_QUIT_KEY")
	if want != "" {
		sc, err := parseChord(want)
		if err == nil {
			err = chordDeliverable(sc)
		}
		if err == nil {
			return sc, want
		}
		fmt.Fprintf(os.Stderr, "vmxplore: VMX_QUIT_KEY %q: %v; using %s\n", want, err, defaultQuitKey)
	}
	sc, _ := parseChord(defaultQuitKey) // a constant that parses; see the test
	return sc, defaultQuitKey
}

// runConsoleGUI shows one VM's screen until the quit chord or the display
// closing. Returns the process exit status.
func runConsoleGUI(name string) int {
	lv, err := ConnectSystem()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmxplore: libvirt: %v\n", err)
		return 1
	}
	port, err := vncPort(lv, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmxplore: %s: %v\n", name, err)
		return 1
	}
	addr, stopTunnel, err := vncEndpoint(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmxplore: %s: %v\n", name, err)
		return 1
	}
	defer stopTunnel()
	conn, err := dialRFB(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmxplore: %s: vnc: %v\n", name, err)
		return 1
	}

	a := app.NewWithID("dev.vmxplore")
	w := a.NewWindow("vmxplore — " + name)
	v := newVNCViewer(conn)
	quitKey, label := resolveQuitKey()
	quit := func() {
		conn.Close()
		a.Quit()
	}
	// both sites, as with the fullscreen chord: a focused viewer forwards the
	// whole keyboard, so a canvas shortcut alone never fires while the guest
	// has the keys
	v.onQuit = quit
	w.Canvas().AddShortcut(quitKey, func(fyne.Shortcut) { quit() })
	conn.SetOnCutText(func(s string) {
		fyne.Do(func() { a.Clipboard().SetContent(s) })
	})
	go func() {
		<-conn.done
		fyne.Do(a.Quit)
	}()
	w.SetContent(v)
	w.SetFullScreen(true)
	w.Canvas().Focus(v)
	fmt.Fprintf(os.Stderr, "vmxplore: %s's screen; %s quits\n", name, label)
	w.ShowAndRun()
	return 0
}
