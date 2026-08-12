//go:build gui

// vnc_race_test.go — the framebuffer-resize race, pinned.
//
// A guest that resizes its display while the operator is clicking is a normal
// event (they resize at boot), and it is the one moment when the read loop
// writes fbW/fbH/img while the Fyne goroutine reads them. That was an
// unsynchronised read in shipped code: harmless-looking, and the kind of thing
// that produces an irreproducible "the console froze" report.
//
// The live VNC test needs a running guest, so it is skipped on CI and could
// never close this. This one talks to a fake RFB server on a local socket, so
// `go test -race -tags gui` exercises the real client — handshake, DesktopSize
// handling, blit — with no libvirt, no guest and no display.
package main

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
)

// fakeRFB serves the 3.8 handshake and then pushes framebuffer updates that
// alternate between a small Raw rectangle and a DesktopSize resize, until the
// test closes stop. Returns the listener address.
func fakeRFB(t *testing.T, stop <-chan struct{}) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		// ── handshake, server side ──────────────────────────────────────
		if _, err := c.Write([]byte(rfbVersion)); err != nil {
			return
		}
		if _, err := io.ReadFull(c, make([]byte, 12)); err != nil { // client version
			return
		}
		if _, err := c.Write([]byte{1, 1}); err != nil { // 1 security type: None
			return
		}
		if _, err := io.ReadFull(c, make([]byte, 1)); err != nil { // chosen type
			return
		}
		if _, err := c.Write([]byte{0, 0, 0, 0}); err != nil { // SecurityResult: OK
			return
		}
		if _, err := io.ReadFull(c, make([]byte, 1)); err != nil { // ClientInit
			return
		}
		init := make([]byte, 24)
		binary.BigEndian.PutUint16(init[0:2], 800) // width
		binary.BigEndian.PutUint16(init[2:4], 600) // height
		binary.BigEndian.PutUint32(init[20:24], 0) // zero-length name
		if _, err := c.Write(init); err != nil {
			return
		}

		// Everything the client sends from here (SetPixelFormat, SetEncodings,
		// update requests, input events) is drained: this server pushes on its
		// own schedule, which is all the client needs to be exercised.
		go func() { _, _ = io.Copy(io.Discard, c) }()

		raw := make([]byte, 16*16*4) // one small BGRX rectangle
		sizes := [][2]uint16{{640, 480}, {800, 600}, {1024, 768}}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var msg []byte
			if i%2 == 0 {
				// Raw 16x16 at the origin — fits every size below.
				msg = make([]byte, 4+12)
				msg[0] = msgFramebufferUpdate
				binary.BigEndian.PutUint16(msg[2:4], 1) // one rect
				binary.BigEndian.PutUint16(msg[8:10], 16)
				binary.BigEndian.PutUint16(msg[10:12], 16)
				binary.BigEndian.PutUint32(msg[12:16], uint32(encRaw))
				msg = append(msg, raw...)
			} else {
				s := sizes[(i/2)%len(sizes)]
				msg = make([]byte, 4+12)
				msg[0] = msgFramebufferUpdate
				binary.BigEndian.PutUint16(msg[2:4], 1)
				binary.BigEndian.PutUint16(msg[8:10], s[0])
				binary.BigEndian.PutUint16(msg[10:12], s[1])
				// -223 on the wire is the two's-complement bit pattern; the
				// client reads it back as int32.
				binary.BigEndian.PutUint32(msg[12:16],
					uint32(int64(encDesktopSize)&0xffffffff))
			}
			if _, err := c.Write(msg); err != nil {
				return
			}
		}
	}()
	return ln.Addr().String()
}

func TestRFBResizeDuringInputIsRaceFree(t *testing.T) {
	stop := make(chan struct{})
	addr := fakeRFB(t, stop)

	conn, err := dialRFB(addr)
	if err != nil {
		close(stop)
		t.Fatalf("dial fake RFB: %v", err)
	}
	if w, h := conn.size(); w != 800 || h != 600 {
		t.Errorf("ServerInit size = %dx%d, want 800x600", w, h)
	}

	// The viewer is built by hand rather than through newVNCViewer: the real
	// constructor installs a callback that hops to the Fyne goroutine, and
	// there is no app running in a unit test. fbCoords is the code under test
	// and needs neither.
	v := &vncViewer{conn: conn}

	deadline := time.Now().Add(400 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				// What a mouse move does, from the UI goroutine, while the
				// read loop is resizing the framebuffer underneath it.
				v.fbCoords(fyne.NewPos(400, 300))
				conn.size()
				conn.frame()
			}
		}()
	}
	wg.Wait()
	close(stop)

	// The client must have survived the resizes: a bad rect, an unrequested
	// encoding or a short read would have ended the loop with an error.
	conn.Close()
	select {
	case <-conn.done:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop did not exit after Close")
	}
	if err := conn.Err(); err != nil {
		t.Fatalf("read loop died: %v", err)
	}
}
