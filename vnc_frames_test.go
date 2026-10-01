//go:build gui

package main

import "testing"

// A burst of frames queues one repaint, not one each; a frame arriving
// after that repaint starts queues exactly one more, so the last frame is
// never lost.
func TestFrameCoalescerQueuesOneRepaint(t *testing.T) {
	var f frameCoalescer
	var queue []func()
	do := func(fn func()) { queue = append(queue, fn) }
	paints := 0
	paint := func() { paints++ }

	for i := 0; i < 1000; i++ {
		f.trigger(do, paint)
	}
	if len(queue) != 1 {
		t.Fatalf("1000 frames queued %d repaints, want 1", len(queue))
	}
	queue[0]() // the UI thread runs it
	if paints != 1 {
		t.Fatalf("paints = %d, want 1", paints)
	}
	f.trigger(do, paint) // a frame after the repaint started
	if len(queue) != 2 {
		t.Fatalf("a frame after the repaint queued %d more, want 1", len(queue)-1)
	}
	queue[1]()
	if paints != 2 {
		t.Fatalf("paints = %d, want 2: the last frame must be painted", paints)
	}
}
