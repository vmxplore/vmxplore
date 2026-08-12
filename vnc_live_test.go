//go:build gui

// vnc_live_test.go — RFB client smoke against a real qemu VNC server.
// Skips unless a running domain with a live VNC display exists, so plain
// `go test` on a dev box stays green; on a host with a running guest it
// proves the handshake, pixel-format negotiation and first framebuffer.
package main

import (
	"fmt"
	"testing"
	"time"
)

func TestRFBLiveFramebuffer(t *testing.T) {
	lv, err := ConnectSystem()
	if err != nil {
		t.Skipf("no libvirt: %v", err)
	}
	defer lv.Close()
	doms, err := lv.Estate()
	if err != nil {
		t.Skipf("estate: %v", err)
	}
	port := 0
	name := ""
	for _, d := range doms {
		if d.State != "running" {
			continue
		}
		if p, err := vncPort(lv, d.Name); err == nil {
			port, name = p, d.Name
			break
		}
	}
	if port == 0 {
		t.Skip("no running domain with a live VNC display")
	}

	conn, err := dialRFB(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	defer conn.Close()
	// Through the accessor: the read loop is already running, so touching
	// conn.fbW directly here is the same race the client itself had.
	fbW, fbH := conn.size()
	if fbW <= 0 || fbH <= 0 {
		t.Fatalf("bad framebuffer size %dx%d", fbW, fbH)
	}

	// A real frame has non-zero pixels somewhere (the guest paints SOMETHING —
	// even a boot console has text).
	//
	// The pixels are inspected INSIDE the frame callback, which the read loop
	// invokes between frames on its own goroutine. Reading img.Pix from the
	// test goroutine instead would race blitRaw's writes, and imgMu would not
	// save it: that mutex guards the framebuffer SWAP, not the pixel writes
	// into the buffer it points at. Same goroutine, no race, no lock.
	type frameInfo struct {
		w, h    int
		nonZero bool
	}
	frames := make(chan frameInfo, 1)
	conn.SetOnFrame(func() {
		info := frameInfo{}
		info.w, info.h = conn.size()
		for _, px := range conn.frame().Pix {
			if px != 0 && px != 0xff { // skip bare alpha
				info.nonZero = true
				break
			}
		}
		select {
		case frames <- info:
		default:
		}
	})
	select {
	case info := <-frames:
		t.Logf("%s: %dx%d framebuffer, non-zero pixels: %v",
			name, info.w, info.h, info.nonZero)
	case <-time.After(5 * time.Second):
		t.Fatal("no framebuffer update within 5s")
	}
}
