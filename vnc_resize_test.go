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
)

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
