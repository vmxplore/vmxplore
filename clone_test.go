package main

import (
	"strings"
	"testing"
)

// The proposed name must be usable as-is: it reaches virt-clone AND `zfs
// clone`, so it has to pass the same validator a typed name does. A default
// the operator has to edit before it works is not a default.
func TestCloneDefaultNameIsValid(t *testing.T) {
	for _, orig := range []string{"klab-blue-centos", "k8s-golden", "app-web-stack", "a"} {
		got := cloneDefaultName(orig)
		if !strings.HasPrefix(got, orig+"-") {
			t.Errorf("%q: proposal %q must extend the original", orig, got)
		}
		if err := validZFSName(got); err != nil {
			t.Errorf("%q: proposal %q is not a valid name: %v", orig, got, err)
		}
		// and the indexed forms a qty > 1 produces
		for _, n := range []string{"-1", "-2", "-15"} {
			if err := validZFSName(got + n); err != nil {
				t.Errorf("%q: indexed %q invalid: %v", orig, got+n, err)
			}
		}
		t.Logf("  %-18s -> %s  (+ -1 .. -N)", orig, got)
	}
}
