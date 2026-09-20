package main

import "testing"

// The split decides whether a row gets forgotten for free or needs --orphans.
// Getting it backwards either destroys a volume nobody asked about, or leaves
// the group uncleanable. Both directions are pinned here.
func TestUnreconciledSplit(t *testing.T) {
	groups := []GroupRows{
		{Label: "klab", Rows: []Row{
			{D: Dom{Name: "live-vm"}}, // healthy, not ours
			{D: Dom{Name: "live-with-ds"}, DS: &Dataset{Name: "rpool/vms/live-with-ds"}},
		}},
		{Label: groupUnreconciled, Rows: []Row{
			{D: Dom{Name: "db-ghost"}, Synthetic: true,
				Notes: []string{"in state.db, not in libvirt"}},
			{D: Dom{Name: "kspawn-ghost"}, Synthetic: true,
				Notes: []string{"in kspawn manifest (c1), not in libvirt"}},
			{D: Dom{Name: "orphan"}, Synthetic: true,
				Backing: "rpool/vms/orphan",
				DS:      &Dataset{Name: "rpool/vms/orphan", Type: "volume"},
				Notes:   []string{"zvol without a domain"}},
		}},
	}
	ghosts, orphans := unreconciledSplit(groups)
	if len(ghosts) != 2 {
		t.Fatalf("ghosts = %d, want 2 (%v)", len(ghosts), names(ghosts))
	}
	if len(orphans) != 1 || orphans[0].D.Name != "orphan" {
		t.Fatalf("orphans = %v, want [orphan]", names(orphans))
	}
	// A healthy row in another group must never be touched.
	for _, r := range append(ghosts, orphans...) {
		if r.D.Name == "live-vm" || r.D.Name == "live-with-ds" {
			t.Fatalf("%s is not unreconciled and must not be selected", r.D.Name)
		}
	}
	t.Logf("ghosts=%v orphans=%v", names(ghosts), names(orphans))
}

func TestUnreconciledSplitEmpty(t *testing.T) {
	g, o := unreconciledSplit([]GroupRows{{Label: "klab", Rows: []Row{{D: Dom{Name: "x"}}}}})
	if len(g) != 0 || len(o) != 0 {
		t.Fatalf("an estate with no unreconciled group must yield nothing, got %d/%d", len(g), len(o))
	}
}

func names(rs []Row) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.D.Name)
	}
	return out
}
