//go:build gui

// gui_capture.go — VMX_CAPTURE=out.png: open the GUI on live data, write a
// PNG of the window, quit.
//
// Why: GNOME on Wayland offers no screenshot path a script can use (grim needs
// wlr-screencopy, which mutter does not implement), and a redesign judged from
// memory of what the window looked like is a guess. The window can capture
// itself, so it does: before/after pictures of the real estate, and the same
// pictures for the docs. VMX_CAPTURE_DELAY (seconds, default 10) is how long
// to let the libvirt and ZFS refreshes land first; the capture is of the
// window as it is then, no state is faked.

package main

import (
	"fmt"
	"image/png"
	"os"
	"strconv"
	"time"

	"fyne.io/fyne/v2"
)

// sel selects a VM by libvirt name, for VMX_CAPTURE_SELECT: the details
// card only shows with a selection, and a capture of the placeholder proves
// nothing about it.
func startCapture(a fyne.App, w fyne.Window, sel func(name string)) {
	out := os.Getenv("VMX_CAPTURE")
	if out == "" {
		return
	}
	delay := 10
	if v, err := strconv.Atoi(os.Getenv("VMX_CAPTURE_DELAY")); err == nil && v > 0 {
		delay = v
	}
	go func() {
		time.Sleep(time.Duration(delay) * time.Second)
		if name := os.Getenv("VMX_CAPTURE_SELECT"); name != "" {
			fyne.Do(func() { sel(name) })
			time.Sleep(2 * time.Second) // let the card and console settle
		}
		fyne.Do(func() {
			img := w.Canvas().Capture()
			f, err := os.Create(out)
			if err == nil {
				err = png.Encode(f, img)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "vmxplore: capture %s: %v\n", out, err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "vmxplore: captured %s (%dx%d)\n", out, img.Bounds().Dx(), img.Bounds().Dy())
			a.Quit()
		})
	}()
}
