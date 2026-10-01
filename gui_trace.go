//go:build gui

// gui_trace.go — VMX_TRACE=1 logs every UI-thread step that takes longer
// than 30 ms, with its name, to stderr.
//
// Why: a click on the sidebar was taking seconds (operator, 2026-09-30) and
// none of the obvious suspects measured slow: BuildEstate is under half a
// millisecond on onyx's 49 domains / 459 datasets / 2,125 snapshots, the
// tree's child lists and row painter read memory only, and zfs list (5 s)
// runs off the UI thread. Anything that runs on the UI thread blocks every
// click behind it, so each such step is wrapped, and the log names the one
// that took the time. Silent and free when VMX_TRACE is unset.

package main

import (
	"log"
	"os"
	"time"
)

var traceOn = os.Getenv("VMX_TRACE") == "1"

// traceSlow is deferred at the top of a UI-thread step:
//
//	defer traceSlow("apply", time.Now())
func traceSlow(what string, start time.Time) {
	if !traceOn {
		return
	}
	if d := time.Since(start); d > 30*time.Millisecond {
		log.Printf("vmxplore trace: %s took %v", what, d.Round(time.Millisecond))
	}
}

// Click-to-paint latency. Every traced step above can be fast while the
// click still takes seconds, because OpenBranch only marks the branch: the
// rows are laid out and painted on a later frame, outside every step. So a
// branch click stamps the time (traceClick), and the next row the tree
// paints (tracePainted) logs how long the operator actually waited, always,
// not only over 30 ms, so a fast click is evidence too.
var (
	clickAt   time.Time
	clickWhat string
)

func traceClick(what string) {
	if traceOn {
		clickAt, clickWhat = time.Now(), what
	}
}

func tracePainted() {
	if !traceOn || clickAt.IsZero() {
		return
	}
	log.Printf("vmxplore trace: click %s -> first row painted after %v",
		clickWhat, time.Since(clickAt).Round(time.Millisecond))
	clickAt = time.Time{}
}
