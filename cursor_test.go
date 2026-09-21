package main

import "testing"

// After a delete the cursor must land on the NEXT VM, not jump to the top or
// onto a group header. rebuild() keeps the cursor by domain NAME, and a
// deleted name matches nothing — which used to fall through to cursor 0.
//
// This drives the REAL rebuild(), not a copy of its placement logic: the
// first version of this test replicated the algorithm and would have passed
// against the broken code.
func TestCursorAfterDelete(t *testing.T) {
	doms := func(names ...string) []Dom {
		var out []Dom
		for _, n := range names {
			out = append(out, Dom{Name: n, State: "shut off"})
		}
		return out
	}
	cur := func(m *ui) string {
		items := m.navItems()
		if m.cursor >= len(items) || items[m.cursor].row < 0 {
			return ""
		}
		it := items[m.cursor]
		return m.groups[it.g].Rows[it.row].D.Name
	}

	cases := []struct {
		name          string
		before, after []string
		onName, want  string
	}{
		{"middle row", []string{"a", "b", "c", "d"}, []string{"a", "c", "d"}, "b", "c"},
		{"first row", []string{"a", "b", "c"}, []string{"b", "c"}, "a", "b"},
		{"last row", []string{"a", "b", "c"}, []string{"a", "b"}, "c", "b"},
		{"only row", []string{"a"}, nil, "a", ""},
		{"survivor keeps its place", []string{"a", "b", "c"}, []string{"a", "b", "c"}, "b", "b"},
	}
	for _, c := range cases {
		rs, rerr := LoadRules("")
		if rerr != nil {
			t.Fatalf("rules: %v", rerr)
		}
		m := &ui{rs: rs, collapsed: map[string]bool{}, ann: &Annotations{}}
		m.doms = doms(c.before...)
		m.rebuild()
		for i, it := range m.navItems() {
			if it.row >= 0 && m.groups[it.g].Rows[it.row].D.Name == c.onName {
				m.cursor = i
			}
		}
		if got := cur(m); got != c.onName {
			t.Fatalf("%s: setup failed, cursor on %q not %q", c.name, got, c.onName)
		}
		m.doms = doms(c.after...) // the delete
		m.rebuild()
		if got := cur(m); got != c.want {
			t.Errorf("%s: cursor landed on %q, want %q", c.name, got, c.want)
		} else {
			t.Logf("  %-24s delete %s -> %q", c.name, c.onName, got)
		}
	}
}
