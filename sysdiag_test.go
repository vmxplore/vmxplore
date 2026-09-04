package main

import (
	"strings"
	"testing"
)

// The report must hold together on any host: three tiers, each requirement
// backed by a probe of the same name (so a "met" can never disagree with
// the card that explains it), and every feature row filled for every tier.
func TestSysdiagShape(t *testing.T) {
	d := RunSysdiag(nil)
	if len(d.Tiers) != 3 {
		t.Fatalf("tiers = %d", len(d.Tiers))
	}
	if SysTierLabel(d.Tier) == d.Tier {
		t.Errorf("tier %q has no label", d.Tier)
	}
	probes := map[string]bool{}
	for _, p := range d.Probes {
		probes[p.Name] = true
	}
	for _, tier := range d.Tiers {
		if len(tier.Has) != len(d.Features) {
			t.Errorf("%s: %d feature marks for %d features", tier.Key, len(tier.Has), len(d.Features))
		}
		for _, r := range tier.Reqs {
			if !probes[r.Name] {
				t.Errorf("%s requires %q, which no probe reports", tier.Key, r.Name)
			}
		}
	}
	// Each tier lights everything the one before it does.
	for i := 1; i < 3; i++ {
		for j := range d.Features {
			if d.Tiers[i-1].Has[j] && !d.Tiers[i].Has[j] {
				t.Errorf("%s loses %q that %s has", d.Tiers[i].Key, d.Features[j], d.Tiers[i-1].Key)
			}
		}
	}
	var sb strings.Builder
	PrintSysdiag(&sb, d)
	if !strings.Contains(sb.String(), "▲ this host") || !strings.Contains(sb.String(), "requirements") {
		t.Errorf("text report missing its marker:\n%s", sb.String())
	}
}
