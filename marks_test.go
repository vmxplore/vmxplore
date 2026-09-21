package main

import "testing"

func mkUI(t *testing.T, names ...string) *ui {
	t.Helper()
	rs, err := LoadRules("")
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	m := &ui{rs: rs, collapsed: map[string]bool{}, marks: map[string]bool{}, ann: &Annotations{}}
	for _, n := range names {
		m.doms = append(m.doms, Dom{Name: n, State: "shut off"})
	}
	m.rebuild()
	return m
}

func (m *ui) cursorTo(t *testing.T, name string) {
	t.Helper()
	for i, it := range m.navItems() {
		if it.row >= 0 && m.groups[it.g].Rows[it.row].D.Name == name {
			m.cursor = i
			return
		}
	}
	t.Fatalf("no row named %q", name)
}

func targetNames(m *ui) []string {
	var out []string
	for _, r := range m.verbTargets() {
		out = append(out, r.D.Name)
	}
	return out
}

func TestMarkOneAndVerbTargets(t *testing.T) {
	m := mkUI(t, "a", "b", "c")
	// nothing marked -> the cursor row
	m.cursorTo(t, "b")
	if got := targetNames(m); len(got) != 1 || got[0] != "b" {
		t.Fatalf("unmarked: targets = %v, want [b]", got)
	}
	// mark two, cursor elsewhere -> the marked ones, in screen order
	m.marks["c"] = true
	m.marks["a"] = true
	got := targetNames(m)
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("marked: targets = %v, want [a c] in screen order", got)
	}
	t.Logf("  unmarked -> cursor row; marked -> %v", got)
}

func TestMarkGroupHeaderToggles(t *testing.T) {
	m := mkUI(t, "a", "b", "c")
	m.cursor = 0 // the group header
	if it := m.navItems()[0]; it.row >= 0 {
		t.Skip("first item is not a header in this grouping")
	}
	m.toggleMark()
	if n := m.markCount(); n != 3 {
		t.Fatalf("header mark: %d marked, want 3", n)
	}
	m.toggleMark() // again -> clears, not a one-way select-all
	if n := m.markCount(); n != 0 {
		t.Fatalf("header re-mark: %d marked, want 0", n)
	}
	t.Log("  header marks all 3, then clears all 3")
}

// A mark naming a VM that no longer exists must not swallow the verb: the
// batch falls back to the cursor row rather than acting on nothing.
func TestStaleMarksFallBackToCursor(t *testing.T) {
	m := mkUI(t, "a", "b")
	m.marks["ghost-that-was-deleted"] = true
	m.cursorTo(t, "a")
	got := targetNames(m)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("stale marks: targets = %v, want [a]", got)
	}
	if len(m.marks) != 0 {
		t.Fatalf("stale marks should have been dropped, %d left", len(m.marks))
	}
	t.Log("  stale mark dropped, verb fell back to the cursor row")
}
