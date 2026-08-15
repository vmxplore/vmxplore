//go:build gui

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestManualRendersWithoutMandocOrMan reproduces a kldload install, which
// ships neither mandoc nor man but does have groff. The viewer used to fall
// straight through to the raw mdoc source, so the Manual pane showed
// ".Sh NAME / .Nm vmxplore / .Xr libvirt 3" to the operator (fiend,
// 2026-08-15).
//
// The absent renderers are simulated with stubs that fail, rather than by
// emptying PATH: groff runs its own helper programs and stripping the
// environment would fail the test for the wrong reason.
func TestManualRendersWithoutMandocOrMan(t *testing.T) {
	if _, err := exec.LookPath("groff"); err != nil {
		t.Skip("no groff on this host to fall back to")
	}
	stub := t.TempDir()
	for _, n := range []string{"mandoc", "man"} {
		if err := os.WriteFile(filepath.Join(stub, n),
			[]byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	out := renderManual()

	// The tell-tales of unrendered source: mdoc macros at line starts.
	for _, macro := range []string{".Sh ", ".Nm ", ".Xr ", ".Fl ", ".Dt "} {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, macro) {
				t.Fatalf("manual is raw mdoc source: found %q", line)
			}
		}
	}
	// ...and the tell-tales of a rendered page.
	for _, want := range []string{"NAME", "SYNOPSIS", "DESCRIPTION"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manual has no %s section", want)
		}
	}
	// Overstrike pairs must be gone: -P -c asks for them precisely so
	// stripOverstrike can remove them, and a leak means bold text renders
	// as "NN AA MM EE".
	if strings.Contains(out, "\x08") {
		t.Error("rendered manual still contains overstrike backspaces")
	}
}
