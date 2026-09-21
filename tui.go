// tui.go — the estate TUI: the joined table, live, keyboard-driven.
//
// bubbletea, matching the family. One screen (the headline view from the
// design doc) with foldable estate groups (←/→, headers are cursor stops)
// plus overlays: detail (enter), snapshots (s, with rollback), actions
// (a — the verb menu; verbs.go builds and GATES the plans), input for the
// verbs that need a name or a size, help (?). ← (or q) backs out of any
// pane — q quits only from the main view — and esc aborts the input prompt.
// There is no confirmation step: every verb runs on its keystroke, and
// firePlan writes the exact argv to the status line as it goes.
//
// Nav: j/k or arrows, pgup/pgdown (ctrl+b/ctrl+f) by a screen, g/home and
// G/end to the ends — in the table and in the snapshot pane alike. Mouse:
// wheel moves the active cursor, click selects, a header click folds,
// re-clicking the selected row opens detail.
//
// Interactive externals run under tea.ExecProcess, which suspends the TUI —
// and also sidesteps DomainOpenConsoleBidirectional's missing abort handle
// (design risk list) until a native console earns its complexity. `c` attaches
// `virsh console` on a libvirt domain and follows `kfire console` on a
// Firecracker microVM, which is not a libvirt domain and would otherwise be a
// lookup for a name libvirt has never heard of. `S` opens ssh to the guest's
// agent-reported IP.
//
// Refresh: libvirt estate every 2s (stats are one bulk RPC); ZFS datasets +
// snapshots every 30s (a 16k-snapshot listing is not a 2s-tick operation).
// CPU%% is the delta of cpu.time between consecutive estate ticks.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─── styles / theme ─────────────────────────────────────────────────────────
// One palette drives the whole tool — table, footer, overlays — so it reads
// as one instrument, not a pile of panes. Two 256-colour sets: dark terminals
// get the arcade variant (neon cyan, magenta, 8-bit green, amber over blue
// rules), light terminals get deep variants that survive a white background.
// Dark is the default and light is opt-in — see wantLightTheme for why that
// asymmetry is deliberate. VMX_THEME=dark|light overrides either way.

var (
	styTitle   lipgloss.Style // app title text
	styGroup   lipgloss.Style // estate group headers (accent, bold)
	styHeader  lipgloss.Style // column header row
	styRunning lipgloss.Style // running rows / ok values
	styOff     lipgloss.Style // shut-off rows / muted values
	styWarn    lipgloss.Style // notes, synthetic rows, warnings
	styCursor  lipgloss.Style // selection bar
	styStatus  lipgloss.Style // status line, hint prose
	styKey     lipgloss.Style // key legends (accent, bold)
	styRule    lipgloss.Style // separator rules
	styCmd     lipgloss.Style // the exact argv, echoed as a verb runs
	styOverlay lipgloss.Style // overlay border box
)

// applyTheme installs one of the two palettes into the shared styles.
//
// The palette is 256-colour and blue-led. It used to be the 8-colour ANSI set,
// where `dim` was colour 8 — "bright black", which on most terminals is a very
// dark grey a shade off the background. That one colour carried BOTH the muted
// text and every separator rule, so the lines that are supposed to give the
// screen its structure were the least visible thing on it, and the whole TUI
// read as dark mush (operator, 2026-09-20: "its pretty dark").
//
// The dark set is deliberately arcade: neon cyan, magenta, 8-bit green and
// amber over blue rules. This is the project that ships a demoscene installer
// — a grey enterprise console would be the wrong house style — and the colours
// are period-correct for a CRT without being a costume.
//
// Three changes, each with a reason:
//   - `rule` is its own colour now, not an alias of `dim`. A separator and a
//     de-emphasised value are different jobs; tying them together meant making
//     one readable made the other shout.
//   - `dim` is a mid grey (245/242) rather than near-black, so muted text is
//     muted rather than absent.
//   - accent and header are different hues, so group headers and column
//     headers are distinguishable at a glance. The old header colour was ANSI
//     12, pure #0000FF, which measures 1.9:1 against a dark background — it
//     was not a dim colour, it was an invisible one.
//
// Every colour here was measured against a #1e1e1e background and clears
// 4.5:1, the WCAG floor for body text; the rule clears it too, which the first
// attempt at this palette did not (a muted blue-grey came out at 2.8:1, i.e.
// LESS visible than the near-black it replaced — measured, not eyeballed).
// `ok` stays green and `warn` stays amber because those two are semantic: a
// running VM that is cyan because cyan is the theme tells the operator less
// than one that is green. lipgloss degrades 256-colour to the nearest ANSI on
// a terminal that cannot do it, so a real VGA console still renders.
func applyTheme(light bool) {
	// accent cyan 13.3:1 · hdr pink 6.6:1 · ok neon green 12.5:1
	// warn amber 9.0:1 · dim grey 4.8:1 · rule blue 4.7:1
	accent, ok, warn, dim, hdr, rule := "51", "82", "214", "245", "207", "68"
	if light {
		accent, ok, warn, dim, hdr, rule = "25", "28", "130", "242", "90", "67"
	}
	styTitle = lipgloss.NewStyle().Bold(true)
	styGroup = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(accent))
	styHeader = lipgloss.NewStyle().Underline(true).Foreground(lipgloss.Color(hdr))
	styRunning = lipgloss.NewStyle().Foreground(lipgloss.Color(ok))
	styOff = lipgloss.NewStyle().Foreground(lipgloss.Color(dim))
	styWarn = lipgloss.NewStyle().Foreground(lipgloss.Color(warn))
	styCursor = lipgloss.NewStyle().Reverse(true).Bold(true)
	// An explicit grey rather than Faint(true): faint is a terminal attribute
	// many emulators render as ~50% alpha over the background, which on a dark
	// theme put the status line and every hint a hair above invisible. A real
	// colour is muted on purpose and still legible.
	styStatus = lipgloss.NewStyle().Foreground(lipgloss.Color(dim))
	styKey = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(accent))
	styRule = lipgloss.NewStyle().Foreground(lipgloss.Color(rule))
	styCmd = lipgloss.NewStyle().Foreground(lipgloss.Color(ok))
	// Double border, not rounded: square double lines are the CRT-era frame
	// and they match the double rule above the footer, so the two framing
	// elements on screen agree with each other.
	styOverlay = lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color(accent)).Padding(0, 1)
}

// dark default so non-TUI paths (status messages built before runTUI, tests)
// never see zero styles; runTUI re-applies with detection + $VMX_THEME
func init() { applyTheme(false) }

// ─── messages & model ───────────────────────────────────────────────────────

type estateMsg struct {
	doms []Dom
	cpu  map[string]uint64
	at   time.Time
	err  error
}

type zfsMsg struct {
	dss   map[string]*Dataset
	snaps map[string][]string
	ann   *Annotations
	err   error
}

type estateTickMsg struct{}
type zfsTickMsg struct{}
type execDoneMsg struct{ err error }
type verbDoneMsg struct {
	title string
	err   error
}

type ui struct {
	lv *LV
	rs *Ruleset

	doms  []Dom
	dss   map[string]*Dataset
	snaps map[string][]string
	ann   *Annotations

	groups    []GroupRows
	collapsed map[string]bool // group label → folded (←/→); survives refresh
	marks     map[string]bool // marked domain NAMES; see marks.go
	cursor    int             // indexes navItems(): group headers AND rows
	scroll    int

	prevCPU map[string]uint64
	prevAt  time.Time
	cpu     map[string]float64

	width, height int
	overlay       string // "" | detail | snaps | help | actions | input
	status        string
	err           error

	// verb state: the plan being run, the operator's typed buffer
	// (the input field), and the staged specs value between the
	// two input rounds. snapCursor selects inside the snaps overlay.
	pending       *verbPlan
	typed         string
	inputKind     string // "snap" | "vcpus" | "mem"
	stagedCPUs    int
	stagedName    string // clone base name, staged between the name and qty rounds
	snapCursor    int
	factoryCursor int    // row in the Factory pane (factory.go)
	factoryScope  string // klab distro scope there; empty means all
}

func newUI(lv *LV, rs *Ruleset) *ui {
	return &ui{lv: lv, rs: rs, cpu: map[string]float64{},
		collapsed: map[string]bool{}, marks: map[string]bool{}, status: "loading estate…"}
}

// navItem is one selectable line of the estate view: a group header
// (row == -1, foldable with ←/→) or a domain row inside an expanded group.
type navItem struct {
	g, row int
}

// navItems lists the selectable lines in render order, honouring folds.
// Rebuilt on demand — the estate is dozens of items, not thousands.
func (m *ui) navItems() []navItem {
	var items []navItem
	for gi, g := range m.groups {
		items = append(items, navItem{gi, -1})
		if m.collapsed[g.Label] {
			continue
		}
		for ri := range g.Rows {
			items = append(items, navItem{gi, ri})
		}
	}
	return items
}

// curRow returns the domain row under the cursor; ok=false when the cursor
// sits on a group header (or the estate is empty). Verbs and overlays that
// need a domain must check ok and complain, not index blindly.
func (m *ui) curRow() (Row, bool) {
	items := m.navItems()
	if m.cursor >= len(items) || items[m.cursor].row < 0 {
		return Row{}, false
	}
	it := items[m.cursor]
	return m.groups[it.g].Rows[it.row], true
}

func (m *ui) Init() tea.Cmd {
	return tea.Batch(m.fetchEstate(), m.fetchZFS())
}

// fetchEstate is the fast tick: libvirt only.
func (m *ui) fetchEstate() tea.Cmd {
	lv := m.lv
	return func() tea.Msg {
		doms, err := lv.Estate()
		if err != nil {
			return estateMsg{err: err}
		}
		cpu := make(map[string]uint64, len(doms))
		for _, d := range doms {
			cpu[d.Name] = d.CPUTimeNs
		}
		return estateMsg{doms: doms, cpu: cpu, at: time.Now()}
	}
}

// fetchZFS is the slow tick: datasets, the 16k-snapshot listing, registers.
func (m *ui) fetchZFS() tea.Cmd {
	return func() tea.Msg {
		msg := zfsMsg{ann: LoadAnnotations()}
		if !HasZFS() {
			return msg
		}
		var err error
		if msg.dss, err = ListDatasets(); err != nil {
			msg.err = err
			return msg
		}
		msg.snaps, err = ListSnapshots()
		msg.err = err
		return msg
	}
}

func after(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// rebuild re-joins after any data change, keeping the cursor on the same
// domain — or the same group header — when it still exists.
func (m *ui) rebuild() {
	var selDom, selHeader string
	prevIdx := m.cursor
	if items := m.navItems(); m.cursor < len(items) {
		it := items[m.cursor]
		if it.row < 0 {
			selHeader = m.groups[it.g].Label
		} else {
			selDom = m.groups[it.g].Rows[it.row].D.Name
		}
	}
	m.groups = withFirecracker(BuildEstate(m.doms, m.dss, m.snaps, m.rs, m.ann))
	m.cursor = 0
	items := m.navItems()
	found := false
	for i, it := range items {
		if it.row < 0 && m.groups[it.g].Label == selHeader ||
			it.row >= 0 && m.groups[it.g].Rows[it.row].D.Name == selDom {
			m.cursor = i
			found = true
			break
		}
	}
	// The selected domain is GONE — almost always because it was just
	// deleted. Stay where the operator was looking and take the row that
	// moved up into the gap, which is the next VM. Falling through to
	// cursor 0 sent them to the top of the estate, onto a group header, with
	// the view scrolled away from whatever they were working through
	// (operator, 2026-09-20: "when you delete something the cursor goes to
	// the bottom, that's annoying, it should just go to the next vm").
	//
	// Nearest row at or after the old index, then nearest before it, so
	// deleting the last VM in the list lands on the new last one rather than
	// on nothing.
	if !found && selDom != "" && len(items) > 0 {
		at := min(prevIdx, len(items)-1)
		m.cursor = at
		for i := at; i < len(items); i++ {
			if items[i].row >= 0 {
				m.cursor = i
				break
			}
		}
		if items[m.cursor].row < 0 {
			for i := at; i >= 0; i-- {
				if items[i].row >= 0 {
					m.cursor = i
					break
				}
			}
		}
	}
	// first load (nothing selected yet): land on the first domain row, not
	// the header above it — the verbs expect a row under the cursor, and
	// "launch, press u" must start a VM, not silently hit a header
	if selDom == "" && selHeader == "" {
		for i, it := range items {
			if it.row >= 0 {
				m.cursor = i
				break
			}
		}
	}
}

func (m *ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case estateMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, after(2*time.Second, estateTickMsg{})
		}
		m.err = nil
		m.doms = msg.doms
		if !m.prevAt.IsZero() {
			wall := msg.at.Sub(m.prevAt)
			s1 := msg.cpu
			m.cpu = cpuPercent(m.prevCPU, s1, wall, msg.doms)
		}
		m.prevCPU, m.prevAt = msg.cpu, msg.at
		m.status = fmt.Sprintf("%d domains · rules: %s · %s",
			len(m.doms), m.rs.Source, time.Now().Format("15:04:05"))
		m.rebuild()
		return m, after(2*time.Second, estateTickMsg{})

	case zfsMsg:
		if msg.err != nil {
			m.status = "zfs: " + msg.err.Error()
		} else {
			m.dss, m.snaps = msg.dss, msg.snaps
		}
		m.ann = msg.ann
		m.rebuild()
		return m, after(30*time.Second, zfsTickMsg{})

	case estateTickMsg:
		return m, m.fetchEstate()
	case zfsTickMsg:
		return m, m.fetchZFS()

	case execDoneMsg:
		if msg.err != nil {
			m.status = styWarn.Render(msg.err.Error())
		}
		// Refresh both halves, exactly as verbDoneMsg does, and for two
		// reasons. The screen: coming back from `c` or `S` this returned a nil
		// command, so nothing drew until the 2 s estate tick happened to fire
		// — the console's last frame just sat there and the TUI looked hung
		// (operator, 2026-09-20: "ctrl d to exit .. it doesnt return you to
		// vmx"). The data: a console session is a session, and a guest that
		// was shut down or rebooted from inside it has a state the table is
		// now wrong about.
		return m, tea.Batch(m.fetchEstate(), m.fetchZFS())

	case verbDoneMsg:
		if msg.err != nil {
			m.status = styWarn.Render(msg.err.Error())
		} else {
			m.status = "done: " + msg.title
		}
		// mutation happened — refresh both halves now, not on the next tick
		return m, tea.Batch(m.fetchEstate(), m.fetchZFS())

	case tea.KeyMsg:
		return m.key(msg)

	case tea.MouseMsg:
		return m.mouse(msg)
	}
	return m, nil
}

// mouse: wheel moves whichever cursor the active view owns; in the main
// view a left click selects (header click folds, clicking the already-
// selected row drills into detail — the poor man's double click).
func (m *ui) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		d := 1
		if msg.Button == tea.MouseButtonWheelUp {
			d = -1
		}
		switch m.overlay {
		case "":
			if n := len(m.navItems()); n > 0 {
				m.cursor = min(max(m.cursor+d, 0), n-1)
			}
		case "snaps":
			if sel, _ := m.snapSelection(); len(sel) > 0 {
				m.snapCursor = min(max(m.snapCursor+d, 0), len(sel)-1)
			}
		}
		return m, nil
	}
	if msg.Action != tea.MouseActionPress ||
		msg.Button != tea.MouseButtonLeft || m.overlay != "" {
		return m, nil
	}
	idx := m.itemAt(msg.Y)
	if idx < 0 {
		return m, nil
	}
	it := m.navItems()[idx]
	switch {
	case it.row < 0:
		l := m.groups[it.g].Label
		m.collapsed[l] = !m.collapsed[l]
		m.cursor = idx
	case idx == m.cursor:
		m.overlay = "detail"
	default:
		m.cursor = idx
	}
	return m, nil
}

// itemAt maps a screen row (0-based, tea.MouseMsg.Y) to a navItems index,
// or -1 when the click lands outside the table body. Must mirror View's
// layout exactly: title, optional libvirt-error line, column header, then
// the scrolled table — change one, change both.
// pageSize is how many table rows fit on screen: the window minus the title,
// the column header, the blank, the rule, the status line and one spare. It is
// what pgup/pgdown move by, and what the table itself scrolls and pads to, so
// a "page" is exactly a screen and the three cannot disagree — the arithmetic
// was written out three times before this existed.
//
// Floored at 3 because a window too short to hold a page still has to move the
// cursor by something, and 0 would make pgdown a no-op that looks like a
// broken key.
func (m *ui) pageSize() int {
	if n := m.height - 6; n > 3 {
		return n
	}
	return 3
}

func (m *ui) itemAt(y int) int {
	top := 2
	if m.err != nil {
		top++
	}
	avail := m.pageSize()
	if y < top || y-top >= avail {
		return -1
	}
	idx := m.scroll + y - top
	if idx >= len(m.navItems()) {
		return -1
	}
	return idx
}

// key routes keystrokes. The model (operator requests 2026-08-07): ← backs
// out of whatever pane → drilled into, q backs out of panes too and only
// quits from the main view, panes also close on the key that opened them
// (enter/?, s, a) — esc is NOT a menu back-out; it survives only as "abort
// this command prompt" in the input overlay, where q has to stay
// typeable for domain names.
func (m *ui) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.overlay {
	case "detail", "help":
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q", "left", "h", "enter", "?":
			m.overlay = ""
		}
		return m, nil
	case "snaps":
		return m.keySnaps(msg)
	case "actions":
		return m.keyActions(msg)
	case "factory":
		return m.keyFactory(msg)
	case "input":
		return m.keyInput(msg)
	}
	items := m.navItems()
	onHeader := m.cursor < len(items) && items[m.cursor].row < 0
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		if m.cursor < len(items)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = max(0, len(items)-1)
	case "pgdown", "ctrl+f":
		// max(0, ...) because an EMPTY estate makes len(items)-1 equal -1, and
		// a cursor of -1 gets past curRow's `>= len(items)` guard and indexes
		// items[-1]. A fresh machine with no VMs is the common case for that.
		m.cursor = max(0, min(len(items)-1, m.cursor+m.pageSize()))
	case "pgup", "ctrl+b":
		m.cursor = max(0, m.cursor-m.pageSize())
	case "left", "h":
		// back out: fold the group under the cursor; from a row, first hop
		// up to its header so a second ← folds
		if m.cursor < len(items) {
			it := items[m.cursor]
			if onHeader {
				m.collapsed[m.groups[it.g].Label] = true
			} else {
				for i := m.cursor; i >= 0; i-- {
					if items[i].row < 0 {
						m.cursor = i
						break
					}
				}
			}
		}
	case "right", "l":
		// drill in: unfold a folded group, step into an open one, and on a
		// row → detail (same as enter)
		if m.cursor < len(items) {
			it := items[m.cursor]
			switch {
			case onHeader && m.collapsed[m.groups[it.g].Label]:
				m.collapsed[m.groups[it.g].Label] = false
			case onHeader && len(m.groups[it.g].Rows) > 0:
				m.cursor++
			case !onHeader:
				m.overlay = "detail"
			}
		}
	case "enter":
		if onHeader { // toggle the fold, same as ←/→
			l := m.groups[items[m.cursor].g].Label
			m.collapsed[l] = !m.collapsed[l]
		} else if _, ok := m.curRow(); ok {
			m.overlay = "detail"
		}
	case " ":
		// Mark, then step down: a run of rows is space-space-space. Stepping
		// only when a ROW was marked — not a whole group from its header —
		// because after marking a group the next useful position is the next
		// group, not its first member.
		items := m.navItems()
		onRow := m.cursor < len(items) && items[m.cursor].row >= 0
		if m.toggleMark() && onRow && m.cursor < len(items)-1 {
			m.cursor++
		}
	case "esc":
		if len(m.marks) > 0 {
			n := m.markCount()
			m.clearMarks()
			m.status = styStatus.Render(fmt.Sprintf("cleared %d mark(s)", n))
		}
	case "s":
		if _, ok := m.curRow(); ok {
			m.overlay = "snaps"
			m.snapCursor = 0
		}
	case "f":
		// Factory: build goldens, run the suites. Not row-scoped — it makes
		// images rather than acting on one — so no curRow check.
		m.overlay = "factory"
		m.factoryCursor = 0
	case "?":
		m.overlay = "help"
	case "a":
		if _, ok := m.curRow(); ok {
			m.overlay = "actions"
		}
	case "c":
		return m, m.execConsole()
	case "S":
		return m, m.execSSH()
	default:
		return m.keyActions(msg) // direct verb keys work without the menu
	}
	return m, nil
}

// keyActions maps a verb key to a plan (from the actions overlay or directly
// from the table). Plans that need typed input route through the input
// overlay first; everything else runs on the keystroke.
func (m *ui) keyActions(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r, ok := m.curRow()
	if !ok {
		// a verb key with a group header under the cursor must say why
		// nothing happened, not swallow the keystroke
		// Every verb key belongs in this set. A key missing from it is
		// swallowed silently on a header row, which reads as "the TUI cannot
		// do that" rather than "move onto a VM first" — the exact impression
		// the unwired verbs already gave.
		if s := msg.String(); len(s) == 1 && strings.ContainsAny(s, "udKApvbzZDFC+=") {
			m.status = styWarn.Render(
				"cursor is on a group header — j/k onto a VM row first")
		}
		m.overlay = ""
		return m, nil
	}
	var plan verbPlan
	var err error
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "left", "h", "a":
		m.overlay = ""
		return m, nil
	// Every verb below dispatches on the row TYPE inside itself: planStart and
	// friends return planFCStart for a Firecracker row, and planDelete returns
	// planFCDelete for one and planReconcile for a synthetic row with no
	// domain behind it. So there is no type test here, and there must not be
	// one — the first cut of this switch branched on r.FC before calling
	// planStart, which re-implemented the routing that lives in the very
	// function it was calling, twenty lines away.
	//
	// These take a PLANNER rather than calling the verb here, so one
	// keystroke can drive many rows: marked rows if any are marked, else the
	// one under the cursor. Everything below this group needs typed input or
	// only makes sense on a single row, and stays single.
	case "u":
		return m.runOnTargets(planStart)
	case "d":
		return m.runOnTargets(planShutdown)
	case "K":
		return m.runOnTargets(planForceOff)
	case "b":
		return m.runOnTargets(planReboot)
	case "z":
		return m.runOnTargets(planSuspend)
	case "Z":
		return m.runOnTargets(planResume)
	case "A":
		return m.runOnTargets(planAutostart)
	case "D":
		// Delete, force-delete a microVM, or forget an unreconciled row —
		// planDelete picks, because "delete" is the verb the operator reaches
		// for on all three.
		return m.runOnTargets(planDelete)
	case "F":
		// Seal a shut-off appliance as a Firecracker golden. Its own key
		// because it is a distinct verb, not a mode of another; planFCGolden
		// refuses rows it does not apply to and its error is shown below.
		plan, err = planFCGolden(r)
	case "C":
		if r.Synthetic {
			m.overlay = ""
			m.status = styWarn.Render("no domain behind this row")
			return m, nil
		}
		// Prefill a name that is already unique, so the common case is
		// enter-enter. The operator wanted "name and qty" and nothing else;
		// making them invent a name for the fifteenth clone of a golden is
		// the opposite of that.
		m.overlay, m.inputKind, m.typed = "input", "clone", cloneDefaultName(r.D.Name)
		return m, nil
	case "+", "=":
		// "+" grows the disk, which is the only direction it can go. "=" is
		// the same physical key unshifted, so it works without reaching for
		// shift — the same courtesy every terminal zoom shortcut extends.
		if r.DS == nil {
			m.overlay = ""
			m.status = styWarn.Render("no local dataset behind " + r.D.Name)
			return m, nil
		}
		m.overlay, m.inputKind, m.typed = "input", "resize", ""
		return m, nil
	case "p":
		if r.DS == nil {
			m.overlay = ""
			m.status = styWarn.Render("no local dataset behind " + r.D.Name)
			return m, nil
		}
		m.overlay, m.inputKind, m.typed = "input", "snap", ""
		return m, nil
	case "v":
		if r.Synthetic {
			m.overlay = ""
			m.status = styWarn.Render("no domain behind this row")
			return m, nil
		}
		m.overlay, m.inputKind, m.typed = "input", "vcpus", ""
		return m, nil
	default:
		return m, nil
	}
	if err != nil {
		m.overlay = ""
		m.status = styWarn.Render(err.Error())
		return m, nil
	}
	return m.toRun(plan, err)
}

// keyInput collects free-text parameters (snapshot suffix, vcpus, memory)
// and hands the finished values to the plan builders.
func (m *ui) keyInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r, ok := m.curRow()
	if !ok {
		m.overlay = ""
		return m, nil
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		m.overlay, m.typed = "", ""
		m.status = "cancelled"
		return m, nil
	case "backspace":
		if len(m.typed) > 0 {
			m.typed = m.typed[:len(m.typed)-1]
		}
		return m, nil
	case "enter":
		switch m.inputKind {
		case "snap":
			plan, err := planSnapshot(r, strings.TrimSpace(m.typed))
			return m.toRun(plan, err)
		case "vcpus":
			n, err := strconv.Atoi(strings.TrimSpace(m.typed))
			if err != nil || n < 1 {
				m.status = styWarn.Render("vcpus must be a positive number")
				return m, nil
			}
			m.stagedCPUs, m.inputKind, m.typed = n, "mem", ""
			return m, nil
		case "mem":
			g, err := strconv.Atoi(strings.TrimSpace(m.typed))
			if err != nil || g < 1 {
				m.status = styWarn.Render("memory must be a positive number of GiB")
				return m, nil
			}
			plan, perr := planSpecs(r, m.stagedCPUs, g)
			return m.toRun(plan, perr)
		case "clone":
			name := strings.TrimSpace(m.typed)
			if name == "" {
				m.status = styWarn.Render("a clone needs a name")
				return m, nil
			}
			// A name reaching virt-clone and `zfs clone` is validated before
			// it gets there, not after: a VM once got created called --help,
			// and the same string pointed at `zfs destroy` is unrecoverable.
			// validZFSName is the check remote.go already applies to every
			// name that reaches a zfs argv — one rule, not a second one here
			// that drifts from it.
			if verr := validZFSName(name); verr != nil {
				m.status = styWarn.Render(verr.Error())
				return m, nil
			}
			// planCloneFrom with fromGolden picks each disk's @golden anchor
			// where one exists and falls back to a fresh snapshot per disk
			// that was never sealed — the same choice the GUI makes, rather
			// than a second rule that drifts from it.
			// Stage it and ask how many. One clone is the common case and
			// costs one more keypress; fifteen is the case that used to mean
			// fifteen trips through this overlay.
			m.stagedName, m.inputKind, m.typed = name, "cloneqty", "1"
			return m, nil
		case "cloneqty":
			n, cerr := strconv.Atoi(strings.TrimSpace(m.typed))
			if cerr != nil || n < 1 {
				m.status = styWarn.Render("how many clones? a positive number")
				return m, nil
			}
			// planCloneFrom with fromGolden picks each disk's @golden anchor
			// where one exists and falls back to a fresh snapshot per disk
			// that was never sealed — the same choice the GUI makes, rather
			// than a second rule that drifts from it.
			if n == 1 {
				plan, perr := planCloneFrom(r, m.stagedName, true)
				return m.toRun(plan, perr)
			}
			// More than one: the typed name is a BASE and each clone gets an
			// index, so the set reads as a set. Suffixing is predictable in a
			// way a fresh random per clone is not — `kldload-w-1` next to
			// `kldload-w-2` is an estate; three unrelated numbers is a mess.
			var plans []verbPlan
			for i := 1; i <= n; i++ {
				nm := fmt.Sprintf("%s-%d", m.stagedName, i)
				if verr := validZFSName(nm); verr != nil {
					m.status = styWarn.Render(verr.Error())
					return m, nil
				}
				p, perr := planCloneFrom(r, nm, true)
				if perr != nil {
					m.status = styWarn.Render(perr.Error())
					return m, nil
				}
				plans = append(plans, p)
			}
			return m.toRunAll(plans)
		case "resize":
			g, err := strconv.Atoi(strings.TrimSpace(m.typed))
			if err != nil || g < 1 {
				m.status = styWarn.Render("size must be a positive number of GiB")
				return m, nil
			}
			// An unknown current size is a refusal, not a default: planResizeDisk
			// treats 0 as "cannot tell" and a wrong guess here would be a shrink,
			// which is one-way.
			cur, cerr := currentDiskBytes(r)
			if cerr != nil {
				m.status = styWarn.Render(cerr.Error())
				return m, nil
			}
			plan, perr := planResizeDisk(r, g, cur)
			return m.toRun(plan, perr)
		}
		return m, nil
	}
	if s := msg.String(); len(s) >= 1 && !strings.HasPrefix(s, "ctrl") {
		m.typed += s
	}
	return m, nil
}

// keySnaps drives the snapshot pane: j/k over the visible non-noise list,
// R plans a rollback of the selected snapshot.
func (m *ui) keySnaps(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sel, all := m.snapSelection()
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "left", "h", "s":
		m.overlay = ""
		return m, nil
	case "j", "down":
		if m.snapCursor < len(sel)-1 {
			m.snapCursor++
		}
	case "k", "up":
		if m.snapCursor > 0 {
			m.snapCursor--
		}
	// The snapshot pane is the one place a list gets genuinely long — a
	// machine with sanoid running carries thousands — so paging matters more
	// here than in the table, not less.
	case "g", "home":
		m.snapCursor = 0
	case "G", "end":
		m.snapCursor = max(0, len(sel)-1)
	case "pgdown", "ctrl+f":
		m.snapCursor = max(0, min(len(sel)-1, m.snapCursor+m.pageSize()))
	case "pgup", "ctrl+b":
		m.snapCursor = max(0, m.snapCursor-m.pageSize())
	case "R":
		r, ok := m.curRow()
		if !ok || m.snapCursor >= len(sel) {
			return m, nil
		}
		target := sel[m.snapCursor]
		newer := 0
		for i, s := range all {
			if s == target {
				newer = len(all) - i - 1
				break
			}
		}
		plan, err := planRollback(r, target, newer)
		return m.toRun(plan, err)
	}
	return m, nil
}

// cloneDefaultName proposes a name that is already free of collisions, so the
// common case is enter, enter.
//
// <original>-HHMMSS. Six digits of wall clock: short enough to read aloud,
// unique enough for a human pressing C, and it sorts chronologically within a
// day — so a list of clones tells you the order they were made in, which a
// random number does not. HHMMSS rather than seconds-of-day because 060562
// looks like a broken clock, while 164922 is one.
//
// It is a PROPOSAL, sitting in the input buffer ready to be edited or
// replaced. Nothing here is forced.
func cloneDefaultName(orig string) string {
	return orig + "-" + time.Now().Format("150405")
}

// toRunAll fires several plans as ONE background command, in order.
//
// Sequential rather than concurrent: every clone of the same source takes a
// snapshot of the same zvol, and N of those racing is a way to find out which
// ZFS operations are not as atomic as assumed. Cloning is fast — a clone is
// metadata — so the wall-clock cost of doing them in turn is small, and a
// failure part way names which one failed rather than leaving the operator to
// work out which of fifteen did not appear.
func (m *ui) toRunAll(plans []verbPlan) (tea.Model, tea.Cmd) {
	if len(plans) == 0 {
		return m, nil
	}
	m.overlay, m.typed, m.pending = "", "", nil
	m.status = styCmd.Render(fmt.Sprintf("running %d clones: %s ...",
		len(plans), plans[0].title))
	return m, func() tea.Msg {
		for i, p := range plans {
			if err := runPlan(p); err != nil {
				return verbDoneMsg{
					title: fmt.Sprintf("%s (clone %d of %d)", p.title, i+1, len(plans)),
					err:   err,
				}
			}
		}
		return verbDoneMsg{title: fmt.Sprintf("%d clones", len(plans))}
	}
}

// toRun executes a plan immediately. There is no confirmation step anywhere
// in this TUI, by operator instruction (2026-09-20: "I dont want any prompts").
//
// What is NOT lost with it: verbs.go still refuses what should be refused —
// a rollback while the domain runs, a shrink, a delete of a row with no
// domain — and those refusals surface as the error below. A prompt was never
// what made those safe. What IS lost is the chance to read the command before
// it runs, so firePlan now writes the exact argv to the status line as it
// goes: the "prints its exact command" contract moves from a box you dismiss
// to a line you can read afterwards.
//
// The input overlay stays. Asking for a clone's name or a new disk size is
// data entry, not confirmation — there is no way to clone without a name.
func (m *ui) toRun(plan verbPlan, err error) (tea.Model, tea.Cmd) {
	if err != nil {
		m.overlay, m.typed = "", ""
		m.status = styWarn.Render(err.Error())
		return m, nil
	}
	m.pending, m.typed, m.overlay = &plan, "", ""
	return m.firePlan()
}

// firePlan runs the pending plan in a Cmd goroutine; the result lands as a
// verbDoneMsg which also forces an immediate estate refresh.
func (m *ui) firePlan() (tea.Model, tea.Cmd) {
	p := *m.pending
	m.pending, m.overlay, m.typed = nil, "", ""
	// The exact command, on screen, as it runs. With the confirm box gone this
	// is the only place the operator sees what a keystroke actually did, and
	// "prints its exact command" is a contract this tool keeps whether or not
	// it asks permission first. Truncated to the width because a verb plan can
	// carry several commands and the status line is one row.
	cmd := strings.Join(strings.Fields(strings.ReplaceAll(p.cmdLines(), "\n", " ; ")), " ")
	m.status = styCmd.Render(truncate(cmd, max(20, m.width-2)))
	if p.warn != "" {
		m.status = styWarn.Render("⚠ "+p.warn) + "  " + m.status
	}
	return m, func() tea.Msg { return verbDoneMsg{title: p.title, err: runPlan(p)} }
}

// ─── external verbs ─────────────────────────────────────────────────────────
// Both print their exact command in the status line — the TUI teaches the
// CLI, never hides it (design: "vmx must never fight virsh").

// rowHasConsole — does this domain have a serial console to attach to?
//
// Asked ONLY when c is pressed, never while rendering: it costs a libvirt XML
// fetch, and the actions menu redraws on every 2 s tick. One call on a
// keystroke is free; one call every two seconds per row is not.
//
// Unknown counts as YES. If libvirt will not hand over the XML, refusing to
// try is worse than attaching and having virsh say why — a guess that blocks
// the operator is a worse failure than a guess that lets them find out.
func (m *ui) rowHasConsole(r Row) bool {
	x, err := m.lv.XML(r.D.Name)
	if err != nil {
		return true
	}
	return strings.Contains(x, "<console") || strings.Contains(x, "<serial")
}

func (m *ui) execConsole() tea.Cmd {
	r, ok := m.curRow()
	if !ok {
		return nil
	}
	if r.Synthetic {
		m.status = styWarn.Render("no domain behind this row")
		return nil
	}
	// A Firecracker microVM is not a libvirt domain, so `virsh console` would
	// look up a name libvirt has never heard of. kfire keeps its own serial
	// log and has a verb to follow it, so route there rather than fail — the
	// same row-type routing the plan* verbs do internally. It follows a log
	// rather than attaching a tty, so ctrl+c is the way out, not ^].
	if r.FC != nil {
		if _, err := exec.LookPath("kfire"); err != nil {
			m.status = styWarn.Render("kfire not found — cannot reach this microVM's console")
			return nil
		}
		m.status = "→ kfire console " + r.D.Name + "   (exit: ctrl+c)"
		hint := "vmxplore: following " + r.D.Name + "'s serial log — ctrl+c to return to vmx"
		args := []string{"-c", `printf '%s\n\n' "$1"; shift; exec "$@"`, "_", hint,
			"kfire", "console", r.D.Name}
		return tea.ExecProcess(exec.Command("/bin/sh", args...),
			func(err error) tea.Msg { return execDoneMsg{err} })
	}
	if _, err := exec.LookPath("virsh"); err != nil {
		m.status = styWarn.Render("virsh not found — install libvirt-client")
		return nil
	}
	// No serial device, no console. libvirt would attach and hang on a domain
	// whose XML has no <console>, which reads as a wedged TUI rather than as
	// "this VM has nothing to attach to". rowHasConsole answers from the XML
	// the estate already holds, so this costs no extra call.
	if !m.rowHasConsole(r) {
		m.status = styWarn.Render(r.D.Name + " has no serial console device — use S for ssh")
		return nil
	}
	// The escape sequence, and why this is worth a paragraph.
	//
	// virsh console leaves on ctrl+] — NOT ctrl+d. ctrl+d is end-of-file: it
	// goes through to the guest's shell, which logs out, and getty hands back
	// a fresh login prompt while virsh stays attached. From the outside that
	// is indistinguishable from "the console will not let me out", which is
	// exactly how it was reported.
	//
	// virsh's own -e takes the escape character, so it is configurable rather
	// than a thing to memorise. Default stays ^] because ctrl+d is genuinely
	// useful INSIDE a guest — ending a here-doc, closing a pipe, logging out
	// — and an escape character is stolen from the guest, not shared with it.
	// Set VMX_CONSOLE_ESCAPE=^D if leaving matters more than sending EOF.
	esc := os.Getenv("VMX_CONSOLE_ESCAPE")
	if esc == "" {
		esc = "^]"
	}
	m.status = "→ virsh console " + r.D.Name + "   (exit: " + esc + ")"
	// pinned URI: bare virsh as a group member lands in qemu:///session
	v := virsh("-e", esc, "console", r.D.Name)
	// The hint goes on the terminal the console is about to take over, because
	// the status line above is the first thing the guest's output covers. sh
	// gets the text and the argv as ARGUMENTS, never interpolated into the
	// script: a domain name reaching a shell is the injection this project
	// does not do.
	hint := "vmxplore: attached to " + r.D.Name + " — press " + esc + " to return to vmx (ctrl+d only logs the guest out)"
	args := append([]string{"-c", `printf '%s\n\n' "$1"; shift; exec "$@"`, "_", hint}, v...)
	c := exec.Command("/bin/sh", args...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return execDoneMsg{err} })
}

// execSSH connects to the guest's first known IPv4 — from the agent when one
// is running, otherwise from the hypervisor's DHCP leases, so an agentless
// cloud image is still one keystroke from a shell. User picked from
// $VMX_SSH_USER; on kldload the installed default is admin.
func (m *ui) execSSH() tea.Cmd {
	r, ok := m.curRow()
	if !ok {
		return nil
	}
	ip := firstIPv4(r.D.IPs)
	if ip == "" {
		m.status = styWarn.Render("no guest IP (no agent, no DHCP lease) — cannot ssh")
		return nil
	}
	user := os.Getenv("VMX_SSH_USER")
	if user == "" && IsKldload() {
		user = "admin"
	}
	dest := ip
	if user != "" {
		dest = user + "@" + ip
	}
	m.status = "→ ssh " + dest
	// Guest ssh does NOT check host keys, and this is the one place in the
	// tool where that is the right answer rather than a shortcut.
	//
	// These guests are clones. kvm-clone, klab and kube-cluster mint a fresh
	// host key on every build, and they draw from a small libvirt DHCP pool,
	// so 192.168.122.101 is a different machine with a different key several
	// times a day. Checking the key against known_hosts produces a warning
	// that is CORRECT every time and USEFUL none of them — and a prompt in
	// front of a key the operator has no way to verify teaches them to press
	// yes without reading, which is worse than not asking.
	//
	// UserKnownHostsFile and GlobalKnownHostsFile go to /dev/null as well, so
	// this never writes an entry that a later, real connection would trip
	// over, and never reads one either.
	//
	// The scope is exactly right and must stay there: short-lived VMs on a
	// private host bridge. The HYPERVISOR connection is a different thing —
	// remote.go's sshFlags keeps StrictHostKeyChecking=accept-new for that,
	// because a durable host whose key changes is news.
	c := exec.Command("ssh", sshGuestArgv(dest)...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return execDoneMsg{err} })
}

func firstIPv4(ips []string) string {
	for _, ip := range ips {
		if !strings.Contains(ip, ":") {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// ─── view ───────────────────────────────────────────────────────────────────

func (m *ui) View() string {
	if m.width == 0 {
		return "loading…"
	}
	var b strings.Builder
	// short semantic version only — the full build number lives in --version
	// (operator: the long form in the title bar is obnoxious)
	title := styKey.Render("vmxplore") + styTitle.Render(" v"+version+" — VM estate")
	if tools := KldloadTools(); len(tools) > 0 {
		title += styStatus.Render("  ·  kldload")
	}
	b.WriteString(title + "\n")
	if m.err != nil {
		b.WriteString(styWarn.Render("libvirt: "+m.err.Error()) + "\n")
	}

	nameW, backW, origW := m.colWidths()
	header := fmt.Sprintf("  %-*s %-13s %-4s %6s %11s  %-*s %-*s %8s %5s %s",
		nameW, "DOMAIN", "STATE", "BOOT", "CPU", "MEM", backW, "BACKING",
		origW, "CLONE OF", "SNAPS", "AGENT", "NOTES")
	b.WriteString(styHeader.Render(truncate(header, m.width)) + "\n")

	lines, cursorLine := m.tableLines()
	avail := m.pageSize()
	if cursorLine < m.scroll {
		m.scroll = cursorLine
	}
	if cursorLine >= m.scroll+avail {
		m.scroll = cursorLine - avail + 1
	}
	end := min(len(lines), m.scroll+avail)
	for _, l := range lines[m.scroll:end] {
		b.WriteString(l + "\n")
	}
	// Pad the table out to the full height so the rule and footer sit ON the
	// bottom edge of the terminal rather than directly under the last VM.
	// Without this an estate shorter than the window drew a frame around the
	// top third of the screen and left the rest an empty black field — which
	// is a large part of why the console read as "too dark" regardless of the
	// palette: most of what was on screen was nothing at all.
	for i := end - m.scroll; i < avail; i++ {
		b.WriteString("\n")
	}

	// Mirror the top of the frame: a blank line and a rule set the menu off
	// from the table the same way the underlined header sets off the title.
	// A double rule rather than a single one: it reads as a frame edge at a
	// glance instead of as another row of content, which is the whole job of
	// the line, and it suits a console that boots a demoscene installer.
	b.WriteString("\n" + styRule.Render(strings.Repeat("═", m.width)) + "\n")
	b.WriteString(m.footerLine())

	if m.overlay != "" {
		return m.renderOverlay(b.String())
	}
	return b.String()
}

// Column widths, minimums. DOMAIN, BACKING and CLONE OF grow into whatever
// the terminal actually has; everything else is a fixed field.
const (
	nameWMin = 22
	backWMin = 26
	origWMin = 28

	// Everything in the row format that is NOT one of the three growable
	// columns: the gutter, STATE, BOOT, CPU, MEM, SNAPS, AGENT and the single
	// spaces between them. Counted from the format string itself —
	//   "%s%-*s %-13s %-4s %6s %11s  %-*s %-*s %8s %5s %s"
	// 2 + 1 + 13 + 1 + 4 + 1 + 6 + 1 + 11 + 2 + 1 + 1 + 8 + 1 + 5 + 1
	fixedCols = 59

	// Room kept for NOTES. Notes are usually empty, so giving them the whole
	// remainder is what made a 200-column terminal render a 135-column table
	// with a third of the screen blank (operator, 2026-09-20: "why is only
	// 2/3 of the screen being used").
	notesW = 24

	// Ceilings. A domain name is not 90 characters and a dataset path is not
	// 120, so past a point extra width buys nothing and a very wide terminal
	// would just spread the table thin.
	nameWMax = 40
	backWMax = 46
	origWMax = 48
)

// colWidths sizes the three growable columns to the terminal.
//
// Below the minimums it returns them unchanged and the row truncates, which is
// the old behaviour and the right one — a narrow terminal should lose the ends
// of long paths, not the columns after them.
func (m *ui) colWidths() (int, int, int) {
	nw, bw, ow := nameWMin, backWMin, origWMin
	extra := m.width - fixedCols - notesW - (nw + bw + ow)
	if extra <= 0 {
		return nw, bw, ow
	}
	// Names first: it is the column the operator reads to find a row, and the
	// one most often truncated in practice (cloudtest-klab-golden-debian is 28).
	grow := func(w, max, share int) int {
		if w+share > max {
			return max
		}
		return w + share
	}
	nw = grow(nw, nameWMax, extra*4/10)
	bw = grow(bw, backWMax, extra*3/10)
	ow = grow(ow, origWMax, extra-extra*4/10-extra*3/10)
	return nw, bw, ow
}

// footerLine renders the status + verb menu with the keys coloured (cyan like
// the group headers, labels faint). Styled output can't go through the
// byte-based truncate(), so when the full line is wider than the terminal it
// falls back to the plain truncated form instead of emitting torn ANSI.
func (m *ui) footerLine() string {
	hints := [...][2]string{
		{"space", "mark"}, {"f", "factory"}, {"enter", "detail"}, {"←→", "fold"}, {"s", "snaps"},
		{"a", "actions"}, {"c", "console"}, {"S", "ssh"}, {"?", "help"}, {"q", "quit"},
	}
	// A live mark count sits in front of the status, because a verb about to
	// hit eleven VMs instead of one is the single most useful thing the
	// footer can say.
	status := m.status
	if n := m.markCount(); n > 0 {
		status = styKey.Render(fmt.Sprintf("%d marked", n)) +
			styStatus.Render(" · esc clears · a verb hits all of them") + "  " + status
	}
	plain := status + "  "
	styled := styStatus.Render(status) + "  "
	for _, h := range hints {
		plain += " [" + h[0] + "]" + h[1]
		styled += " " + styKey.Render("["+h[0]+"]") + styStatus.Render(h[1])
	}
	if lipgloss.Width(styled) > m.width {
		return styStatus.Render(truncate(plain, m.width))
	}
	return styled
}

// tableLines renders the nav items (group headers + rows of expanded groups)
// to styled lines and reports which line the cursor landed on (for
// scrolling). Headers carry the fold arrow: ▾ open, ▸ folded.
func (m *ui) tableLines() ([]string, int) {
	nameW, backW, origW := m.colWidths()
	var lines []string
	cursorLine := 0
	for i, it := range m.navItems() {
		var line string
		sty := lipgloss.NewStyle()
		if it.row < 0 {
			g := m.groups[it.g]
			arrow := "▾"
			suffix := ""
			if m.collapsed[g.Label] {
				arrow = "▸"
				if n := runningIn(g); n > 0 {
					suffix = fmt.Sprintf(" · %d running", n)
				}
			}
			nm := 0
			for _, r := range g.Rows {
				if m.marked(r.D.Name) {
					nm++
				}
			}
			if nm > 0 {
				suffix += styKey.Render(fmt.Sprintf(" · %d marked", nm))
			}
			line = fmt.Sprintf("%s %s (%d)%s", arrow, g.Label, len(g.Rows), suffix)
			sty = styGroup
		} else {
			r := m.groups[it.g].Rows[it.row]
			// The two leading spaces are the mark gutter. A marked row shows
			// its marker there rather than recolouring the line, because the
			// line's colour already means something — running, shut off,
			// warning — and a second meaning on the same channel makes both
			// harder to read.
			gutter := "  "
			if m.marked(r.D.Name) {
				gutter = styKey.Render("> ")
			}
			line = fmt.Sprintf("%s%-*s %-13s %-4s %6s %11s  %-*s %-*s %8s %5s %s",
				gutter, nameW, truncate(r.D.Name, nameW), r.D.State, bootCell(r),
				cpuCell(m.cpu, r), memCell(r),
				backW, truncate(cellOr(r.Backing, "-"), backW),
				origW, truncate(cellOr(shortOrigin(r.Origin), "-"), origW),
				snapCell(r), agentCell(r), strings.Join(r.Notes, "; "))
			switch {
			case r.Synthetic || len(r.Notes) > 0:
				sty = styWarn
			case r.D.State == "running":
				sty = styRunning
			case r.D.State == "shut off":
				sty = styOff
			}
		}
		line = truncate(line, m.width)
		if i == m.cursor {
			cursorLine = len(lines)
			line = styCursor.Render(line)
		} else {
			line = sty.Render(line)
		}
		lines = append(lines, line)
	}
	return lines, cursorLine
}

// runningIn counts running domains in a group — shown on folded headers so
// a collapsed group never hides live workload.
func runningIn(g GroupRows) int {
	n := 0
	for _, r := range g.Rows {
		if r.D.State == "running" {
			n++
		}
	}
	return n
}

func (m *ui) renderOverlay(base string) string {
	var content string
	switch m.overlay {
	case "detail":
		content = m.detailText()
	case "snaps":
		content = m.snapsText()
	case "help":
		content = helpText()
	case "actions":
		content = m.actionsText()
	case "factory":
		content = m.factoryText()
	case "input":
		content = m.inputText()
	}
	// Overlay width scales with the terminal instead of sitting at a fixed 76.
	//
	// 76 was the whole rule, so on a 200-column screen the actions menu and
	// the help pane used 38% of it and wrapped text that had room not to —
	// the same fixed-width mistake the table had (operator, 2026-09-20:
	// "can make the menu bigger too").
	//
	// Three-fifths rather than the full width: an overlay is a thing ON TOP
	// of the estate, and seeing the table either side of it is what says so.
	// The floor stays 76 because that is the width the longest help and menu
	// lines were written against, and the ceiling keeps a margin so the
	// double border never touches the edge.
	boxW := m.width * 3 / 5
	if boxW < 76 {
		boxW = 76
	}
	if boxW > m.width-8 {
		boxW = m.width - 8
	}
	if boxW < 24 {
		boxW = 24 // a terminal too narrow for any of this; render something
	}
	box := styOverlay.Width(boxW).Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		box, lipgloss.WithWhitespaceChars(" "))
}

// keyHint renders "key text · key text" overlay-footer hints in the shared
// palette: accent keys, faint prose. Args alternate key, text.
func keyHint(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts,
			styKey.Render(pairs[i])+" "+styStatus.Render(pairs[i+1]))
	}
	return strings.Join(parts, styStatus.Render(" · "))
}

// detailLine is one "label value" row of the detail pane: the attribute
// name in the accent colour so it stands out (operator ask), the value
// muted to match the main window — bright default-white read as glare next
// to the styled table. Pre-styled values (state, warnings) pass through.
func detailLine(label, val string) string {
	if !strings.Contains(val, "\x1b") {
		val = styOff.Render(val)
	}
	return styKey.Render(fmt.Sprintf("%-8s", label)) + " " + val + "\n"
}

func (m *ui) detailText() string {
	r, ok := m.curRow()
	if !ok {
		return keyHint("←/enter", "close")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", styGroup.Render(r.D.Name))
	state := r.D.State
	switch state {
	case "running":
		state = styRunning.Render(state)
	case "shut off":
		state = styOff.Render(state)
	}
	b.WriteString(detailLine("state", state))
	if !r.Synthetic {
		b.WriteString(detailLine("uuid", r.D.UUID))
		b.WriteString(detailLine("vcpu/mem",
			fmt.Sprintf("%d / %s", r.D.VCPUs, memCell(r))))
		auto, pers := "no", "persistent"
		if r.D.Autostart {
			auto = styRunning.Render("yes")
		}
		if !r.D.Persistent {
			pers = styWarn.Render("TRANSIENT — gone after destroy")
		}
		b.WriteString(detailLine("config", "autostart "+auto+" · "+pers))
		for _, d := range r.D.Disks {
			b.WriteString(detailLine("disk",
				d.Target+" → "+cellOr(d.Dev, d.File)))
		}
		if len(r.D.IPs) > 0 {
			b.WriteString(detailLine("ip", strings.Join(r.D.IPs, ", ")))
		}
	}
	if r.DS != nil {
		b.WriteString(detailLine("dataset", fmt.Sprintf("%s  (used %s, refer %s)",
			r.DS.Name, humanBytes(r.DS.Used), humanBytes(r.DS.Refer))))
		if chain := OriginChain(r.DS, m.dss); len(chain) > 0 {
			b.WriteString(detailLine("lineage", strings.Join(chain, " ← ")))
		}
	}
	for _, n := range r.Notes {
		b.WriteString(detailLine("note", styWarn.Render(n)))
	}
	b.WriteString("\n" + keyHint("←/enter", "close"))
	return b.String()
}

// snapSelection returns the selectable (visible, non-noise) snapshots for
// the current row in render order, plus the dataset's FULL snapshot list in
// creation order — the latter is what rollback's "destroys N newer" math
// must count, noise included.
func (m *ui) snapSelection() (sel []string, all []string) {
	r, ok := m.curRow()
	if !ok || r.DS == nil {
		return nil, nil
	}
	all = m.snaps[r.DS.Name]
	byClass := make(map[string][]string)
	for _, s := range all {
		c := m.rs.ClassifySnap(s)
		byClass[c] = append(byClass[c], s)
	}
	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		if c != SnapNoise {
			classes = append(classes, c)
		}
	}
	sort.Strings(classes)
	for _, c := range classes {
		list := byClass[c]
		if len(list) > snapCapPerClass {
			list = list[len(list)-snapCapPerClass:]
		}
		sel = append(sel, list...)
	}
	return sel, all
}

const snapCapPerClass = 12

// snapsText is the classified snapshot pane: noise collapsed to one line,
// every other class listed (newest last, capped for the box), cursor-
// selectable for rollback.
func (m *ui) snapsText() string {
	r, ok := m.curRow()
	if !ok || r.DS == nil {
		// nil dss = the slow ZFS tick hasn't returned yet; don't claim
		// "no dataset" when the truth is "not looked yet"
		if m.dss == nil {
			return "ZFS data still loading…\n\n" + keyHint("←/s", "close")
		}
		return "no local dataset behind this row\n\n" + keyHint("←/s", "close")
	}
	sel, all := m.snapSelection()
	if m.snapCursor >= len(sel) {
		m.snapCursor = max(0, len(sel)-1)
	}
	byClass := make(map[string][]string)
	for _, s := range all {
		c := m.rs.ClassifySnap(s)
		byClass[c] = append(byClass[c], s)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s\n\n", styGroup.Render(r.DS.Name),
		styStatus.Render(fmt.Sprintf(" — %d snapshots", len(all))))
	if n := len(byClass[SnapNoise]); n > 0 {
		fmt.Fprintf(&b, "%s\n", styStatus.Render(
			fmt.Sprintf("%d automated (noise) — collapsed", n)))
	}
	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		if c != SnapNoise {
			classes = append(classes, c)
		}
	}
	sort.Strings(classes)
	idx := 0
	for _, c := range classes {
		list := byClass[c]
		fmt.Fprintf(&b, "\n%s\n", styGroup.Render(fmt.Sprintf("▾ %s (%d)", c, len(list))))
		shown := list
		if len(shown) > snapCapPerClass {
			shown = shown[len(shown)-snapCapPerClass:]
			b.WriteString(styStatus.Render(fmt.Sprintf(
				"  … %d older omitted", len(list)-snapCapPerClass)) + "\n")
		}
		for _, s := range shown {
			line := "  @" + s
			if idx == m.snapCursor {
				line = styCursor.Render(line)
			}
			b.WriteString(line + "\n")
			idx++
		}
	}
	b.WriteString("\n" + keyHint("j/k", "select", "R", "rollback", "←/s", "close"))
	return b.String()
}

// actionsText is the verb menu for the selected row.
func (m *ui) actionsText() string {
	r, ok := m.curRow()
	if !ok {
		return keyHint("←/a", "close")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s\n\n", styGroup.Render(r.D.Name),
		styStatus.Render(" — actions"))
	auto := styOff.Render("off")
	if r.D.Autostart {
		auto = styRunning.Render("on")
	}
	verb := func(key, desc string) {
		b.WriteString("  " + styKey.Render(key) + "  " + desc + "\n")
	}
	// Grouped, because a flat list of fifteen keys is a wall. The operator
	// scans for the JOB first — power, disk, access — and the letter second.
	sect := func(name string) {
		b.WriteString("\n" + styHeader.Render(name) + "\n")
	}
	// A microVM answers the same keys with kfire verbs (the plan* functions
	// route on row type), so say which kind of row this is instead of making
	// the operator remember the rule.
	if r.FC != nil {
		b.WriteString(styStatus.Render("  firecracker microVM — power verbs use kfire\n"))
	}

	sect("POWER")
	verb("u", "start")
	verb("d", "shut down (graceful)")
	verb("b", "reboot")
	verb("z", "suspend (pause)")
	verb("Z", "resume")
	verb("K", "force off "+styWarn.Render("(no undo)"))

	sect("DISK")
	verb("p", "snapshot (zfs, manual-*)")
	verb("s", "snapshot pane — browse, and R rolls back")
	verb("C", "clone — name, then how many "+styStatus.Render("(enter accepts both)"))
	verb("+", "grow the disk "+styWarn.Render("(one way)"))

	sect("CONFIG")
	verb("v", "vcpu / memory (next start)")
	verb("A", "autostart (now: "+auto+")")
	verb("F", "seal as a firecracker golden")

	sect("ACCESS")
	// Name the console this row will actually get. A microVM follows a kfire
	// log and leaves on ctrl+c; a libvirt domain attaches a tty and leaves on
	// ^]. One label for both would be wrong for one of them.
	if r.FC != nil {
		verb("c", "serial log — kfire "+styStatus.Render("(exit ctrl+c)"))
	} else {
		verb("c", "serial console "+styStatus.Render("(exit ctrl+])"))
	}
	verb("S", "ssh to the guest")
	verb("enter", "detail — disks, addresses, ZFS lineage")

	sect("DANGER")
	verb("D", "delete "+styWarn.Render("(domain + zvol + its snapshots)"))

	b.WriteString("\n" + styWarn.Render("every verb runs on the keystroke — nothing asks twice") + "\n" +
		styStatus.Render("keys work from the table too; this menu is a reminder, not a mode · ") +
		keyHint("←/a", "close"))
	return b.String()
}

// inputText is the parameter prompt for snapshot / specs.
func (m *ui) inputText() string {
	r, ok := m.curRow()
	if !ok {
		return keyHint("esc", "cancel")
	}
	var prompt string
	switch m.inputKind {
	case "snap":
		prompt = fmt.Sprintf("snapshot %s@manual-<suffix>\nsuffix (empty = timestamp):",
			styTitle.Render(r.DS.Name))
	case "vcpus":
		prompt = fmt.Sprintf("new vCPU count for %s (now %d):",
			styTitle.Render(r.D.Name), r.D.VCPUs)
	case "mem":
		prompt = fmt.Sprintf("new memory for %s in GiB (now %s):",
			styTitle.Render(r.D.Name), humanBytes(r.D.MaxMemKiB*1024))
	case "clone":
		prompt = fmt.Sprintf("clone %s — name (enter accepts):", styTitle.Render(r.D.Name))
	case "cloneqty":
		prompt = fmt.Sprintf("how many clones of %s? (>1 appends -1, -2, ...):",
			styTitle.Render(m.stagedName))
	case "resize":
		// The current size is read when the verb runs, not here: asking the
		// hypervisor on every keystroke of the prompt would shell out per
		// character.
		prompt = fmt.Sprintf("grow %s to, in GiB (one way — a disk cannot be shrunk back):",
			styTitle.Render(r.D.Name))
	}
	return prompt + "\n" + styKey.Render("  >") + " " + m.typed + "\n\n" +
		keyHint("enter", "continue", "esc", "cancel")
}

// helpText builds the ?-overlay: sectioned like the estate table, keys in
// the same accent as the footer menu so the whole tool reads as one palette.
func helpText() string {
	var b strings.Builder
	section := func(name string) {
		b.WriteString(styGroup.Render("▾ "+name) + "\n")
	}
	k := func(key, desc string) {
		b.WriteString("  " + styKey.Render(fmt.Sprintf("%-9s", key)) +
			" " + desc + "\n")
	}
	b.WriteString(styKey.Render("vmxplore") +
		styTitle.Render(" v"+version+" — keys") + "\n\n")
	section("navigate")
	k("j/k ↑/↓", "move (group headers select too)")
	k("pgup/pgdn", "a screen at a time (ctrl+b / ctrl+f)")
	k("f", "factory — build goldens (klab lean/GNOME/Xfce/KDE/db, k8s, appliances), run the suites")
	k("space", "mark a row (on a group header: the whole group); a verb then hits every marked row")
	k("esc", "clear every mark")
	k("← →", "fold / unfold the group under the cursor")
	k("g/G · home/end", "top / bottom")
	k("mouse", "wheel scrolls · click selects · header click folds ·")
	k("", "clicking the selected row opens detail")
	k("r", "refresh now")
	k("q", "quit — main view only; inside a pane it backs out")
	b.WriteString("\n")
	section("inspect")
	k("enter", "domain detail (disks, IPs, ZFS lineage)")
	k("s", "snapshots, classified (noise collapsed; R rolls back)")
	k("c", "serial console — virsh, exit ctrl+]; microVM rows follow the kfire log, exit ctrl+c")
	k("S", "ssh to guest (agent IP; $VMX_SSH_USER)")
	b.WriteString("\n")
	section("act — a opens the menu, or press the verb key directly")
	k("u", "start — microVM rows use the kfire verb")
	k("d", "shut down (graceful)")
	k("K", "force off "+styWarn.Render("(no undo)"))
	k("b", "reboot")
	k("z/Z", "suspend / resume")
	k("p", "snapshot (zfs, manual-*)")
	k("C", "clone — proposes <name>-HHMMSS, then asks how many; >1 appends -1, -2, ...")
	k("+", "grow the disk "+styWarn.Render("(one way)"))
	k("v", "edit vcpu/mem (next start)")
	k("A", "autostart toggle")
	k("F", "seal as a firecracker golden")
	k("D", "delete — or forget an unreconciled row "+styWarn.Render("(zvol + snapshots)"))
	b.WriteString("\n" + styStatus.Render(
		"Panes close with ← / q / the key that opened them; esc only\n"+
			"aborts a command prompt. Every mutation shows its exact\n"+
			"virsh/zfs command and asks first; all runs land in the audit\n"+
			"log (/var/log/kldload/vmx.log).") + "\n\n" +
		keyHint("←/?", "close"))
	return b.String()
}

func truncate(s string, w int) string {
	if w <= 0 || len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[:w]
	}
	return s[:w-1] + "…"
}

// wantLightTheme decides which palette to install, and defaults to DARK on
// any uncertainty rather than to light.
//
// It used to be `light := !lipgloss.HasDarkBackground()`, which reads "light
// unless proven dark" — and that detection asks the terminal a question that
// goes unanswered over plenty of ssh sessions, inside tmux, and on a plain
// TTY. Unanswered came out as light, and the light palette on a dark terminal
// is not merely wrong, it is unreadable: its colours measure 1.9:1 to 4.4:1
// against #1e1e1e, where 4.5:1 is the floor for body text. The whole console
// looked like it had been dipped in mud, which is exactly the report
// (operator, 2026-09-20: "the colors still suck, too dark").
//
// So light is now opt-IN: taken only when something actually says so. A dark
// palette on a light terminal is merely ugly; a light palette on a dark one is
// unusable, and unusable is the worse failure to default to.
func wantLightTheme() bool {
	// An explicit choice always wins, and is the documented escape hatch when
	// the guess below is wrong.
	switch os.Getenv("VMX_THEME") {
	case "light":
		return true
	case "dark":
		return false
	}
	// COLORFGBG is "fg;bg" with bg an ANSI index — 0-6 and 8 are dark, 7 and
	// 9-15 are light. Terminals that set it are telling us outright, so it is
	// worth more than a query that may time out.
	if fgbg := os.Getenv("COLORFGBG"); fgbg != "" {
		if i := strings.LastIndex(fgbg, ";"); i >= 0 {
			switch strings.TrimSpace(fgbg[i+1:]) {
			case "7", "9", "10", "11", "12", "13", "14", "15":
				return true
			default:
				return false
			}
		}
	}
	// Last resort: the terminal query. termenv, underneath, falls back to a
	// dark background when it cannot get an answer, so an unanswered query
	// lands on dark — which is the side to fail to. The two checks above
	// exist because that fallback is an implementation detail of a dependency
	// and not a promise, and because COLORFGBG is a direct answer where a
	// query is a guess.
	return !lipgloss.HasDarkBackground()
}

func runTUI(lv *LV, rs *Ruleset) error {
	applyTheme(wantLightTheme())
	p := tea.NewProgram(newUI(lv, rs), tea.WithAltScreen(),
		tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}
