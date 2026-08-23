package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The gate must never claim a capability the host lacks. These assert the
// REASONING, not this machine's answer, so the test is not a snapshot of
// whichever box CI happens to run on.
func TestAvailableNeverGatesOnKldloadIdentity(t *testing.T) {
	// CapNone is unconditional — every existing un-annotated action.
	if ok, why := Available(CapNone); !ok {
		t.Fatalf("CapNone must always be available, got %q", why)
	}
}

func TestUnavailableAlwaysExplainsItself(t *testing.T) {
	for _, c := range []Capability{CapKVM, CapZFS, CapKlab} {
		ok, why := Available(c)
		if !ok && why == "" {
			t.Errorf("capability %d unavailable with no reason — a grey tile must say why", c)
		}
		if ok && why != "" {
			t.Errorf("capability %d available but carries reason %q", c, why)
		}
	}
}

func TestTierIsNonEmpty(t *testing.T) {
	if Tier() == "" {
		t.Fatal("Tier() must always name a tier for the status line")
	}
}

// A host with klab but no ZFS must NOT be offered golden builds: klab writes
// every golden to a zvol. This is the ordering that matters in Available.
func TestKlabWithoutZFSIsRefused(t *testing.T) {
	if !HasKlab() {
		t.Skip("no klab on this host — ordering already exercised by the ZFS branch")
	}
	if !HasZFS() {
		if ok, _ := Available(CapKlab); ok {
			t.Fatal("klab present but no ZFS must be unavailable")
		}
	}
}

// Every feature row must describe all three worlds. A blank cell is the bug
// this table exists to prevent — "no answer" reads as "no", and for Clone that
// would be wrong: plain KVM CAN clone, it just pays a full copy for it.
func TestFeatureMatrixHasNoBlankCells(t *testing.T) {
	if len(FeatureMatrix) == 0 {
		t.Fatal("FeatureMatrix is empty")
	}
	for _, f := range FeatureMatrix {
		if f.Name == "" {
			t.Error("feature with no name")
		}
		if f.KVM == "" || f.KVMZFS == "" || f.Kldload == "" {
			t.Errorf("%q has a blank cell (KVM=%q ZFS=%q kldload=%q) — say what it does, not nothing",
				f.Name, f.KVM, f.KVMZFS, f.Kldload)
		}
	}
}

// kldload is a superset: nothing may be possible on a lesser tier and absent
// on the greater one. Catches a row edited in one column only.
func TestKldloadNeverLosesACapability(t *testing.T) {
	for _, f := range FeatureMatrix {
		if f.Kldload == "no" && f.KVMZFS != "no" {
			t.Errorf("%q: KVM+ZFS can do it but kldload cannot — tiers inverted", f.Name)
		}
		if f.KVMZFS == "no" && f.KVM != "no" {
			t.Errorf("%q: plain KVM can do it but KVM+ZFS cannot — tiers inverted", f.Name)
		}
	}
}

func TestTierColumnMatchesAMatrixColumn(t *testing.T) {
	switch c := TierColumn(); c {
	case "", "KVM", "KVM+ZFS", "kldload":
	default:
		t.Fatalf("TierColumn() = %q, not a column of FeatureMatrix", c)
	}
}

// A GUI session's PATH does not include /usr/local/sbin, which is exactly
// where kldload installs kldload-db. Before this, every caller used LookPath
// alone and read the miss as "not a kldload host", so clones stamped out from
// the GUI were never registered and nothing said so (.120, 2026-08-22).
func TestFindToolFallsBackToAbsolutePathWhenPATHMisses(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kldload-db")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// An empty PATH guarantees LookPath cannot find it, which is the whole
	// point: the fallback is the only thing that can succeed here.
	t.Setenv("PATH", "")

	if got := findTool("kldload-db", bin); got != bin {
		t.Errorf("fallback not used: got %q, want %q", got, bin)
	}
	if got := findTool("kldload-db", filepath.Join(dir, "absent")); got != "" {
		t.Errorf("a genuinely missing tool must report empty, got %q", got)
	}
	// A directory at the path, or a non-executable file, is not a usable tool.
	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, []byte("x"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if got := findTool("kldload-db", noexec); got != "" {
		t.Errorf("non-executable file must not count as a tool, got %q", got)
	}
}
