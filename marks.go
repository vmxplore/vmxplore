// marks.go — multi-select: mark rows, then drive a verb across all of them.
//
// WHY: up-arrow-u is genuinely fast on three VMs and hopeless on fifteen. An
// estate of five klab clones per site, twelve appliance tiles and six cluster
// nodes is normal here, and "shut the klab site down" was fifteen keystrokes
// with no way to see what was already done (operator, 2026-09-20: "need a way
// to mark or group things to turn off lots or start").
//
// The model, and why it is by NAME rather than by index:
//
//	space         mark/unmark the row under the cursor, then step down, so a
//	              run of rows is space-space-space
//	space on a
//	group header  mark or unmark that whole group — the "group things"
//	              half of the request, and the reason sites and lineages are
//	              groups in the first place
//	esc           clear every mark
//	any verb      applies to the marked rows if there are any, else to the
//	              row under the cursor
//
// Marks live in a set of domain NAMES, not row indices, because the estate
// rebuilds every two seconds and indices move under you: a VM that finishes
// booting changes its group, and an index that meant "klab-blue-centos" a
// tick ago means something else now. Names survive that. They also survive a
// delete correctly — the name simply stops matching anything, which is what
// "it is gone" should mean.
package main

import tea "github.com/charmbracelet/bubbletea"

// marked reports whether a domain is marked.
func (m *ui) marked(name string) bool { return m.marks[name] }

// markCount is how many marks are live. Marks for domains that no longer
// exist are not counted: a deleted VM should not keep a batch alive.
func (m *ui) markCount() int {
	n := 0
	for _, g := range m.groups {
		for _, r := range g.Rows {
			if m.marks[r.D.Name] {
				n++
			}
		}
	}
	return n
}

// toggleMark flips the row under the cursor, or every row of the group when
// the cursor is on a header. Returns false when the cursor is on neither,
// which only happens on an empty estate.
func (m *ui) toggleMark() bool {
	items := m.navItems()
	if m.cursor >= len(items) {
		return false
	}
	if m.marks == nil {
		m.marks = map[string]bool{}
	}
	it := items[m.cursor]
	if it.row < 0 {
		// A header: mark the whole group, or clear it if every row is
		// already marked — so space on a header is a toggle like every other
		// space, not a one-way "select all".
		rows := m.groups[it.g].Rows
		all := len(rows) > 0
		for _, r := range rows {
			if !m.marks[r.D.Name] {
				all = false
				break
			}
		}
		for _, r := range rows {
			if all {
				delete(m.marks, r.D.Name)
			} else {
				m.marks[r.D.Name] = true
			}
		}
		return true
	}
	name := m.groups[it.g].Rows[it.row].D.Name
	if m.marks[name] {
		delete(m.marks, name)
	} else {
		m.marks[name] = true
	}
	return true
}

// clearMarks drops every mark.
func (m *ui) clearMarks() { m.marks = map[string]bool{} }

// verbTargets is what a verb should act on: the marked rows in the order they
// appear on screen, or the row under the cursor when nothing is marked.
//
// Screen order matters. "Shut down the klab site" issued bottom-up reads as a
// random walk in the log; issued in the order the operator sees makes the
// batch legible while it runs.
func (m *ui) verbTargets() []Row {
	var out []Row
	if len(m.marks) > 0 {
		for _, g := range m.groups {
			for _, r := range g.Rows {
				if m.marks[r.D.Name] {
					out = append(out, r)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
		// Every mark referred to a domain that no longer exists. Fall through
		// to the cursor rather than silently doing nothing, and drop the
		// stale set so the next verb is not surprising either.
		m.clearMarks()
	}
	if r, ok := m.curRow(); ok {
		out = append(out, r)
	}
	return out
}

// runOnTargets plans one verb across every target and runs them in order.
//
// Planning happens for ALL targets before ANY of them runs. verbs.go refuses
// what should be refused — a rollback while the domain is running, a delete of
// a transient domain — and a batch that discovers its ninth refusal after
// eight have already gone is worse than one that never started. So a single
// planning error aborts the whole batch and says which row caused it.
func (m *ui) runOnTargets(plan func(Row) (verbPlan, error)) (tea.Model, tea.Cmd) {
	targets := m.verbTargets()
	if len(targets) == 0 {
		m.overlay = ""
		m.status = styWarn.Render("nothing to act on")
		return m, nil
	}
	plans := make([]verbPlan, 0, len(targets))
	for _, r := range targets {
		p, err := plan(r)
		if err != nil {
			m.overlay = ""
			m.status = styWarn.Render(r.D.Name + ": " + err.Error())
			return m, nil
		}
		plans = append(plans, p)
	}
	// The marks have been spent. Leaving them set means the next keystroke
	// silently repeats across the same batch, which is how an operator force
	// offs fifteen VMs while intending to force off one.
	m.clearMarks()
	if len(plans) == 1 {
		return m.toRun(plans[0], nil)
	}
	return m.toRunAll(plans)
}
