//go:build gui

// vnc_extsize_live_test.go — does the server on the other end actually let us
// drive the guest's resolution?
//
// requestSize fails silently when the server never advertised
// ExtendedDesktopSize, which is correct in production (a console that works
// beats one that errors about an optimisation) and useless when the picture
// is letterboxed and you need to know WHICH half is broken: the client not
// asking, the server not offering, or the guest not following.
//
// Gated on VMX_VNC_LIVE=<host:port> because it needs a running guest:
//
//	VMX_VNC_LIVE=127.0.0.1:5900 go test -tags gui -run LiveExtended -v .

package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestLiveExtendedDesktopSize(t *testing.T) {
	addr := os.Getenv("VMX_VNC_LIVE")
	if addr == "" {
		t.Skip("set VMX_VNC_LIVE=host:port to probe a running guest")
	}
	conn, err := dialRFB(addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	w, h := conn.size()
	t.Logf("connected: framebuffer %dx%d", w, h)

	// The server announces support by sending an ExtendedDesktopSize rect,
	// which arrives with the first update — give it a moment to land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn.extMu.Lock()
		ok, id := conn.extOK, conn.screenID
		conn.extMu.Unlock()
		if ok {
			t.Logf("server SUPPORTS ExtendedDesktopSize (screen id %#x)", id)
			// Ask for something clearly different and see whether the guest
			// follows within a few seconds.
			// VMX_VNC_WANT=WxH asks for a specific mode, so the same probe
			// can distinguish "the guest refuses every resize" from "the
			// guest refuses modes its driver does not advertise".
			want, wantH := 1920, 1080
			if s := os.Getenv("VMX_VNC_WANT"); s != "" {
				if _, err := fmt.Sscanf(s, "%dx%d", &want, &wantH); err != nil {
					t.Fatalf("VMX_VNC_WANT=%q: want WxH: %v", s, err)
				}
			}
			if w == want && h == wantH {
				want, wantH = 1600, 900
			}
			t.Logf("requesting %dx%d ...", want, wantH)
			conn.requestSize(want, wantH)
			until := time.Now().Add(5 * time.Second)
			for time.Now().Before(until) {
				if gw, gh := conn.size(); gw == want && gh == wantH {
					t.Logf("guest RESIZED to %dx%d", gw, gh)
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			gw, gh := conn.size()
			t.Fatalf("server accepted the encoding but the framebuffer is "+
				"still %dx%d — the request was sent and the guest did not "+
				"follow (driver cannot change mode, or the server refused)",
				gw, gh)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("server never sent an ExtendedDesktopSize rect: it does not " +
		"support client-driven resize, so the letterbox is expected here")
}
