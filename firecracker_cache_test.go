//go:build gui

package main

import (
	"testing"
	"time"
)

// The UI thread's read of the goldens must not wait for a slow kfire. It
// used to: fcGoldensCached held fcMu through `kfire goldens` (4-6 s on onyx)
// and fcGoldensSnapshot, called from the tree painters, took the same lock.
func TestFCGoldensSnapshotDoesNotWaitForKfire(t *testing.T) {
	oldI, oldG, oldH := fcInstancesRead, fcGoldensRead, fcHaveKfire
	defer func() { fcInstancesRead, fcGoldensRead, fcHaveKfire = oldI, oldG, oldH }()
	fcMu.Lock()
	fcAt, fcCached, fcGoldenC, fcGoldenAt = time.Time{}, nil, nil, time.Time{}
	fcMu.Unlock()

	started := make(chan struct{})
	fcHaveKfire = func() bool { return true }
	fcInstancesRead = func() ([]FCInstance, error) { return nil, nil }
	fcGoldensRead = func() ([]FCGolden, error) {
		close(started)
		time.Sleep(2 * time.Second) // a slow kfire goldens
		return []FCGolden{{Name: "g1"}}, nil
	}
	done := make(chan []FCGolden)
	go func() { done <- fcGoldensCached() }() // the background refresh
	<-started
	t0 := time.Now()
	_ = fcGoldensSnapshot() // what a tree painter does, mid-refresh
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Fatalf("fcGoldensSnapshot waited %v for kfire; it must not block", d)
	}
	if gs := <-done; len(gs) != 1 || gs[0].Name != "g1" {
		t.Fatalf("refresh returned %v, want [g1]", gs)
	}
	if gs := fcGoldensSnapshot(); len(gs) != 1 {
		t.Fatalf("snapshot after the refresh = %v, want the new goldens", gs)
	}
}
