package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The gate must exclude GOROUTINES (the mutex) and PROCESSES (the flock).
// The second is the one that matters: kfire runs one `vmx --enroll` per
// machine, and forty of those racing on allocMeshSubnet hand the same /24 to
// several guests.
func TestEnrollGateExcludesGoroutines(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var mu sync.Mutex
	live, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g := enrollHostLock()
			defer g.unlock()
			mu.Lock()
			live++
			if live > peak {
				peak = live
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			live--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Errorf("%d holders at once — the gate does not exclude", peak)
	}
}

// Two PROCESSES must not hold it at once. flock(2) is what makes that true,
// so the test takes the lock here and asserts a child cannot.
func TestEnrollGateExcludesProcesses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	g := enrollHostLock()
	if g.f == nil {
		g.unlock()
		t.Skip("no lock file could be taken on this host")
	}
	path := g.f.Name()
	defer g.unlock()
	// flock in a child: it must fail while we hold it.
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock(1) not available")
	}
	out, err := exec.Command("flock", "-n", path, "true").CombinedOutput()
	if err == nil {
		t.Fatalf("a second process took the lock while it was held: %s", out)
	}
	// and must succeed once released
	g.unlock()
	if out, err := exec.Command("flock", "-n", path, "true").CombinedOutput(); err != nil {
		t.Fatalf("lock not released: %v %s", err, out)
	}
	g.f = nil // already unlocked; keep the deferred unlock harmless
	enrollHostMu.Lock()
}

// A host where no lock file can be created must still enroll, on the mutex
// alone, rather than refuse.
func TestEnrollGateDegradesWithoutAFile(t *testing.T) {
	dir := t.TempDir()
	deny := filepath.Join(dir, "deny")
	if err := os.MkdirAll(deny, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", deny)
	t.Setenv("PATH", dir) // no sudo on PATH, so the fallback create fails too
	g := enrollHostLock()
	defer g.unlock()
	if g.f != nil && !strings.HasPrefix(g.f.Name(), "/run/") {
		t.Errorf("unexpectedly took a lock at %s", g.f.Name())
	}
}
