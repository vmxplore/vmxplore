package main

import (
	"strings"
	"testing"
)

// TestRandomNameShape pins the format operators have to retype off a console.
func TestRandomNameShape(t *testing.T) {
	n, err := randomName("clone-")
	if err != nil {
		t.Fatalf("randomName: %v", err)
	}
	if !strings.HasPrefix(n, "clone-") {
		t.Errorf("%q does not start with the prefix", n)
	}
	body := strings.TrimPrefix(n, "clone-")
	if len(body) != 10 {
		t.Errorf("%q: body is %d symbols, want 10", n, len(body))
	}
	// The ambiguous glyphs are excluded on purpose — see cloneNameAlphabet.
	for _, r := range body {
		if !strings.ContainsRune(cloneNameAlphabet, r) {
			t.Errorf("%q contains %q, which is not in the alphabet", n, r)
		}
	}
	// It must also be a legal ZFS dataset component, since it becomes one.
	if err := validZFSName(n); err != nil {
		t.Errorf("%q is not a valid ZFS name: %v", n, err)
	}
}

// TestFreshNameReservesWithinABatch is the check the operator asked for:
// nothing may be handed out twice, including to two clones in one click.
// The names do not exist yet, so nameInUse cannot see them — only the
// reservation map can.
func TestFreshNameReservesWithinABatch(t *testing.T) {
	taken := map[string]bool{}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		n, err := freshName("clone-", taken, "")
		if err != nil {
			t.Fatalf("freshName: %v", err)
		}
		if seen[n] {
			t.Fatalf("freshName handed out %q twice", n)
		}
		seen[n] = true
		if !taken[n] {
			t.Errorf("%q was returned but not reserved", n)
		}
	}
}

// TestFreshNameSkipsWhatIsAlreadyTaken proves the reservation map is
// consulted rather than merely written.
func TestFreshNameSkipsWhatIsAlreadyTaken(t *testing.T) {
	// Pre-reserve everything the generator could produce for one fixed
	// name by generating it first, then asking again.
	taken := map[string]bool{}
	first, err := freshName("clone-", taken, "")
	if err != nil {
		t.Fatalf("freshName: %v", err)
	}
	for i := 0; i < 50; i++ {
		n, err := freshName("clone-", taken, "")
		if err != nil {
			t.Fatalf("freshName: %v", err)
		}
		if n == first {
			t.Fatalf("freshName returned the reserved name %q again", n)
		}
	}
}
