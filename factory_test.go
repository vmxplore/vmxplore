package main

import (
	"strings"
	"testing"
)

// The pane is a MENU over shipped verbs. What it must get right is the argv:
// a wrong one either does nothing or builds the wrong thing for an hour.
func TestFactoryArgv(t *testing.T) {
	rows := factoryRows()
	if len(rows) < 8 {
		t.Fatalf("catalogue looks truncated: %d rows", len(rows))
	}
	m := &ui{}
	// Default scope is every distro.
	for _, it := range rows {
		got := strings.Join(m.factoryArgv(it), " ")
		if it.scoped && !strings.HasSuffix(got, " all") {
			t.Errorf("%s: scoped entry must default to all, got %q", it.label, got)
		}
		if !it.scoped && strings.HasSuffix(got, " all") {
			t.Errorf("%s: unscoped entry must not take a distro, got %q", it.label, got)
		}
	}
	// A narrowed scope reaches only the klab entries.
	m.factoryScope = "fedora"
	for _, it := range rows {
		got := strings.Join(m.factoryArgv(it), " ")
		if it.scoped && !strings.HasSuffix(got, " fedora") {
			t.Errorf("%s: scope not applied, got %q", it.label, got)
		}
		if !it.scoped && strings.Contains(got, "fedora") {
			t.Errorf("%s: scope leaked into an unscoped entry: %q", it.label, got)
		}
	}
	for _, it := range rows {
		t.Logf("  %-22s %s", it.label, strings.Join(m.factoryArgv(it), " "))
	}
}

// Every distro key the pane advertises must be one klab actually knows.
func TestFactoryScopesAreRealDistros(t *testing.T) {
	known := map[string]bool{}
	for _, d := range klabDistros {
		known[d] = true
	}
	for _, d := range []string{"centos", "rocky", "fedora", "debian", "ubuntu"} {
		if !known[d] {
			t.Errorf("%s offered but not in klabDistros", d)
		}
	}
	if len(klabDistros) != 5 {
		t.Errorf("klabDistros drifted from klab's own DISTROS: %v", klabDistros)
	}
}
