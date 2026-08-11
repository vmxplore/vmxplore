package main

import "testing"

// The flat list is derived from the groups, so a tool can never exist in one
// and not the other — which is what would happen if both were hand-kept.
func TestFlatListMatchesTheGroups(t *testing.T) {
	n := 0
	seen := map[string]string{}
	for _, g := range vmKToolGroups {
		for _, tool := range g.Tools {
			if prev, dup := seen[tool]; dup {
				t.Errorf("%s appears in both %q and %q", tool, prev, g.Name)
			}
			seen[tool] = g.Name
			n++
		}
	}
	if len(vmKTools) != n {
		t.Errorf("flat list has %d tools, groups have %d", len(vmKTools), n)
	}
	for _, tool := range vmKTools {
		if _, ok := seen[tool]; !ok {
			t.Errorf("%s is in the flat list but no group", tool)
		}
	}
}

// Sections exist to make the tab scannable. A section big enough to need
// scanning itself has not helped, and one with a single tile is a heading
// wearing a tile as a hat.
func TestSectionsAreScannable(t *testing.T) {
	for _, g := range vmKToolGroups {
		if len(g.Tools) > 6 {
			t.Errorf("%q has %d tools — split it", g.Name, len(g.Tools))
		}
		if len(g.Tools) < 3 {
			t.Errorf("%q has only %d tools — merge it", g.Name, len(g.Tools))
		}
	}
}
