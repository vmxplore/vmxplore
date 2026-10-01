//go:build gui

package main

import (
	"io"
	"net"
	"testing"
	"time"
)

func queuedConn(c net.Conn) *rfbConn {
	r := &rfbConn{c: c, done: make(chan struct{}), out: make(chan []byte, 256), stopW: make(chan struct{})}
	go r.writeLoop()
	return r
}

// A guest that stops reading must not stall the caller (the Fyne goroutine):
// motion is dropped once the queue is full, and a button press gives up
// after about a second with an error instead of hanging or vanishing.
func TestVNCInputNeverBlocksOnAStalledGuest(t *testing.T) {
	client, server := net.Pipe() // server never reads: every Write blocks
	defer server.Close()
	r := queuedConn(client)
	defer r.Close()

	r.pointer(0, 1, 1) // first event: establishes the mask
	start := time.Now()
	for i := 0; i < 1000; i++ {
		r.pointer(0, i%640, i%480) // pure motion
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("1000 motion events took %v with a stalled guest; the UI thread blocked", d)
	}
	if r.Err() != nil {
		t.Fatalf("dropping motion must not fail the connection: %v", r.Err())
	}
	start = time.Now()
	r.pointer(1, 5, 5) // a press: never dropped
	d := time.Since(start)
	if d < 900*time.Millisecond || d > 2*time.Second {
		t.Fatalf("a press with a full queue returned after %v, want ~1s", d)
	}
	if r.Err() == nil {
		t.Fatal("a press that could not be queued must be reported as an error")
	}
}

// A guest that reads gets every press, move and release, in order.
func TestVNCInputArrivesInOrder(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	r := queuedConn(client)
	defer r.Close()

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 18)
		if _, err := io.ReadFull(server, buf); err == nil {
			got <- buf
		}
		close(got)
	}()
	r.pointer(1, 10, 20) // press
	r.pointer(1, 11, 21) // drag
	r.pointer(0, 11, 21) // release
	select {
	case b := <-got:
		if b == nil {
			t.Fatal("server read failed")
		}
		masks := []byte{b[1], b[7], b[13]}
		if masks[0] != 1 || masks[1] != 1 || masks[2] != 0 {
			t.Fatalf("button masks arrived as %v, want [1 1 0]", masks)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events never reached the guest")
	}
}
