//go:build gui

// vnc_resize_test.go — the SetDesktopSize request, byte for byte.
//
// This is protocol code with hand-written offsets, and a wrong offset here
// does not crash: qemu reads a well-formed-looking message with nonsense in
// it and either ignores the request or resizes to something absurd, which
// looks exactly like "the guest does not support resize". A test that reads
// the bytes back is the only thing that tells those two apart.

package main

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"fyne.io/fyne/v2"
)

// Fyne swallows the standard editing chords before they reach TypedRune and
// hands them to TypedShortcut instead. A handler that ignores one does not
// pass it through — it eats it, silently. That is how Ctrl+C stopped being
// able to interrupt a process in a guest terminal: no error, no log line, the
// keystroke simply never arrived. These assert the bytes on the wire.
func TestEditingShortcutsReachTheGuest(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   fyne.Shortcut
		sym  uint32
	}{
		{"copy/interrupt", &fyne.ShortcutCopy{}, 'c'},
		{"cut", &fyne.ShortcutCut{}, 'x'},
		{"select all", &fyne.ShortcutSelectAll{}, 'a'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, peer := pipeConn(t)
			v := &vncViewer{conn: r}

			go v.TypedShortcut(tc.sc)

			if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			// Two KeyEvents, 8 bytes each: press then release.
			buf := make([]byte, 16)
			if _, err := io.ReadFull(peer, buf); err != nil {
				t.Fatalf("nothing reached the guest: %v", err)
			}
			for i, want := range []byte{1, 0} { // down, then up
				off := i * 8
				if buf[off] != msgKeyEvent {
					t.Errorf("event %d: type = %d, want KeyEvent", i, buf[off])
				}
				if buf[off+1] != want {
					t.Errorf("event %d: down flag = %d, want %d", i, buf[off+1], want)
				}
				if got := binary.BigEndian.Uint32(buf[off+4 : off+8]); got != tc.sym {
					t.Errorf("event %d: keysym = %#x, want %#x (the UNMODIFIED "+
						"letter — the guest applies the Control it already holds)",
						i, got, tc.sym)
				}
			}
		})
	}
}

// pipeConn wires an rfbConn to an in-memory peer so a write can be read back.
func pipeConn(t *testing.T) (*rfbConn, net.Conn) {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { _ = ours.Close(); _ = theirs.Close() })
	return &rfbConn{c: ours, done: make(chan struct{})}, theirs
}

func TestRequestSizeWireFormat(t *testing.T) {
	r, peer := pipeConn(t)
	// Pretend the server announced support and gave us a screen to echo.
	r.extOK, r.screenID, r.screenFlags = true, 0xdeadbeef, 0
	r.fbW, r.fbH = 1280, 720

	go r.requestSize(1920, 1080)

	buf := make([]byte, 24)
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatalf("no SetDesktopSize arrived: %v", err)
	}

	if buf[0] != msgSetDesktopSize {
		t.Errorf("message type = %d, want %d", buf[0], msgSetDesktopSize)
	}
	if got := binary.BigEndian.Uint16(buf[2:4]); got != 1920 {
		t.Errorf("width = %d, want 1920", got)
	}
	if got := binary.BigEndian.Uint16(buf[4:6]); got != 1080 {
		t.Errorf("height = %d, want 1080", got)
	}
	if buf[6] != 1 {
		t.Errorf("screen count = %d, want 1", buf[6])
	}
	if got := binary.BigEndian.Uint32(buf[8:12]); got != 0xdeadbeef {
		t.Errorf("screen id = %#x, want the one the server advertised", got)
	}
	// The single screen must cover the whole framebuffer, or a server that
	// honours the layout gives you the size you asked for with the guest
	// painting into a corner of it.
	if x := binary.BigEndian.Uint16(buf[12:14]); x != 0 {
		t.Errorf("screen x = %d, want 0", x)
	}
	if y := binary.BigEndian.Uint16(buf[14:16]); y != 0 {
		t.Errorf("screen y = %d, want 0", y)
	}
	if got := binary.BigEndian.Uint16(buf[16:18]); got != 1920 {
		t.Errorf("screen width = %d, want 1920", got)
	}
	if got := binary.BigEndian.Uint16(buf[18:20]); got != 1080 {
		t.Errorf("screen height = %d, want 1080", got)
	}
}

// A server that never sent an ExtendedDesktopSize rect has not agreed to be
// resized. Asking anyway is a protocol violation against a peer that may
// simply drop the connection — silence is the correct behaviour.
func TestRequestSizeSilentWithoutServerSupport(t *testing.T) {
	r, peer := pipeConn(t)
	r.extOK = false
	r.fbW, r.fbH = 1280, 720

	done := make(chan struct{})
	go func() { r.requestSize(1920, 1080); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("requestSize blocked writing to a server that never opted in")
	}

	if err := peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := peer.Read(b[:]); err == nil {
		t.Fatal("sent SetDesktopSize to a server that never advertised support")
	}
}

// Asking for the size the guest already is would cost a real mode change —
// a black flash and a driver reprobe — for no change at all. The debounced
// resize path calls this on every settle, so the guard matters.
func TestRequestSizeSkipsNoOp(t *testing.T) {
	r, peer := pipeConn(t)
	r.extOK = true
	r.fbW, r.fbH = 1920, 1080

	done := make(chan struct{})
	go func() { r.requestSize(1920, 1080); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("requestSize blocked on a no-op resize")
	}

	if err := peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := peer.Read(b[:]); err == nil {
		t.Fatal("asked the guest to resize to the size it already was")
	}
}

// The fullscreen chord is the one key a guest can never receive, so parsing
// it wrong is not a cosmetic failure: a typo that resolved to something
// plausible would silently steal a key from every guest, and one that
// resolved to nothing would leave fullscreen with no way out.
func TestParseChord(t *testing.T) {
	ok := map[string]struct {
		key fyne.KeyName
		mod fyne.KeyModifier
	}{
		"alt+delete":   {fyne.KeyDelete, fyne.KeyModifierAlt},
		"ALT+Del":      {fyne.KeyDelete, fyne.KeyModifierAlt},
		"alt+f11":      {fyne.KeyF11, fyne.KeyModifierAlt},
		"ctrl+shift+f": {"F", fyne.KeyModifierControl | fyne.KeyModifierShift},
		"super+space":  {fyne.KeySpace, fyne.KeyModifierSuper},
	}
	for in, want := range ok {
		sc, err := parseChord(in)
		if err != nil {
			t.Errorf("parseChord(%q) failed: %v", in, err)
			continue
		}
		if sc.KeyName != want.key || sc.Modifier != want.mod {
			t.Errorf("parseChord(%q) = %v/%v, want %v/%v",
				in, sc.KeyName, sc.Modifier, want.key, want.mod)
		}
	}
	// A bare key is refused on purpose: it would be taken from the guest
	// with no way left to type it. Shift-only chords are refused for a
	// different reason — Fyne never delivers them as custom shortcuts, so
	// they are unbindable however well they parse. This case used to assert
	// that "shift+f11" was ACCEPTED, which is how a dead default shipped;
	// see chordDeliverable and gui_keys_test.go.
	for _, bad := range []string{
		"f11", "", "hyper+f1", "alt+nosuchkey", "alt+", "shift+f11",
	} {
		if _, err := parseChord(bad); err == nil {
			t.Errorf("parseChord(%q) accepted, want an error", bad)
		}
	}
}

// The default must parse, or the fallback path in resolveFullScreenKey has
// nothing to fall back to.
func TestDefaultChordParses(t *testing.T) {
	if _, err := parseChord(defaultFullScreenChord); err != nil {
		t.Fatalf("the shipped default does not parse: %v", err)
	}
}

// The SetEncodings message, decoded back into the numbers it is supposed to
// carry. The first version of this message hand-wrote -308 as 0xfffffed4,
// which is -300; qemu ignored the unknown encoding, never advertised
// ExtendedDesktopSize, and the resize feature was dead on arrival with no
// symptom other than a letterboxed picture. Two's complement by hand is the
// bug; asserting the round trip is the guard.
func TestSetEncodingsCarriesTheRightNumbers(t *testing.T) {
	r, peer := pipeConn(t)
	go func() { _ = r.setEncodings() }()

	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(peer, head); err != nil {
		t.Fatalf("no SetEncodings: %v", err)
	}
	if head[0] != msgSetEncodings {
		t.Fatalf("message type = %d, want %d", head[0], msgSetEncodings)
	}
	n := int(binary.BigEndian.Uint16(head[2:4]))
	body := make([]byte, 4*n)
	if _, err := io.ReadFull(peer, body); err != nil {
		t.Fatalf("short body: %v", err)
	}
	got := make(map[int32]bool, n)
	for i := 0; i < n; i++ {
		got[int32(binary.BigEndian.Uint32(body[i*4:i*4+4]))] = true
	}
	for _, want := range []int32{encRaw, encDesktopSize, encExtendedDesktopSize} {
		if !got[want] {
			t.Errorf("encoding %d (%#x) missing from SetEncodings; sent %v",
				want, uint32(want), got)
		}
	}
}
