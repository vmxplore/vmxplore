// factory.go — the Factory pane: build goldens and run the suites, from the TUI.
//
// WHY: every image this estate is made of already has a shipped verb — `klab
// golden`, `klab golden-desktop`, `kube-cluster golden`, `vmx --build-all` —
// and the operator was typing all of them by hand while looking at a console
// that already knows what exists and what does not. "I want to make them in
// the TUI, it's simple and fast" (2026-09-20), and the same argument the
// Image Factory note makes: the catalogue is push-button or it is a wiki page
// nobody opens.
//
// WHAT IT IS NOT: a second implementation. Every entry here is an argv for a
// tool that ships and is tested on its own; the pane is a menu, not an engine.
// Re-implementing `klab golden` here would be the mistake the engineering
// rules name outright — and klab's build-all already holds a lock, tracks
// what exists, and skips what is built, none of which is worth copying.
//
// HOW IT RUNS: under tea.ExecProcess, like `c` and `S`. A golden build is ten
// to sixty minutes of output the operator wants to WATCH — a spinner would be
// worse than the thing it hid — and ctrl+c reaches the build rather than the
// TUI. When it ends, execDoneMsg refreshes the estate, so a finished golden
// appears in the table without a keystroke.
//
// SCOPE: the klab entries build every distro by default. Pressing a distro
// key narrows them, which is the difference between "rebuild the lot" and
// "just fedora again" — the single most common thing an operator wants from
// this menu and the reason it is one line of state rather than five copies of
// every entry.
package main

import (
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// klabDistros mirrors klab's own DISTROS. Kept as a literal because it is a
// MENU, and a menu that shells out to ask what to offer is a menu that hangs
// on a broken tool. klab refuses an unknown distro anyway, which is the check
// that matters.
var klabDistros = []string{"centos", "rocky", "fedora", "debian", "ubuntu"}

// factoryItem is one row of the pane: a label, the argv it runs, and whether
// the klab distro scope applies to it.
type factoryItem struct {
	section string // non-empty starts a new section and this row is a header
	label   string
	note    string
	argv    []string
	scoped  bool // append the distro scope (or "all")
}

// factoryItems is the catalogue. Order is the order it renders.
func factoryItems() []factoryItem {
	return []factoryItem{
		{section: "GOLDENS — klab"},
		{label: "lean", note: "minimal per-distro base", argv: []string{"klab", "golden"}, scoped: true},
		{label: "GNOME desktop", note: "instant desktop clones", argv: []string{"klab", "golden-desktop"}, scoped: true},
		{label: "Xfce desktop", note: "lighter desktop", argv: []string{"klab", "golden-xfce"}, scoped: true},
		{label: "KDE Plasma", note: "", argv: []string{"klab", "golden-kde"}, scoped: true},
		{label: "PostgreSQL", note: "carries a generated database", argv: []string{"klab", "golden-db"}, scoped: true},

		{section: "GOLDENS — other"},
		{label: "kubernetes golden", note: "kubeadm, containerd, helm, cilium-cli", argv: []string{"kube-cluster", "golden"}},
		{label: "appliance catalogue", note: "all 12 tiles, kept as Firecracker goldens", argv: []string{"vmx", "--build-all"}},

		{section: "TESTS"},
		{label: "kubernetes smoke", note: "against the running cluster", argv: []string{"kube-smoke-test"}},
		{label: "kldload suite", note: "every shipped smoke suite", argv: []string{"kldload-test"}},
		{label: "zfs test", note: "", argv: []string{"kzfs-test"}},
	}
}

// factoryRows is the catalogue minus the section headers, in render order —
// what the cursor actually moves over.
func factoryRows() []factoryItem {
	var out []factoryItem
	for _, it := range factoryItems() {
		if it.section == "" {
			out = append(out, it)
		}
	}
	return out
}

// factoryArgv builds the command for the selected row, applying the scope.
func (m *ui) factoryArgv(it factoryItem) []string {
	argv := append([]string{}, it.argv...)
	if it.scoped {
		if m.factoryScope == "" || m.factoryScope == "all" {
			argv = append(argv, "all")
		} else {
			argv = append(argv, m.factoryScope)
		}
	}
	return argv
}

// keyFactory drives the pane: j/k to move, a distro letter to scope, enter to
// run, q/left to close.
func (m *ui) keyFactory(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := factoryRows()
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "left", "h", "f":
		m.overlay = ""
		return m, nil
	case "j", "down":
		if m.factoryCursor < len(rows)-1 {
			m.factoryCursor++
		}
	case "k", "up":
		if m.factoryCursor > 0 {
			m.factoryCursor--
		}
	case "g", "home":
		m.factoryCursor = 0
	case "G", "end":
		m.factoryCursor = max(0, len(rows)-1)
	case "a":
		m.factoryScope = "all"
	case "c":
		m.factoryScope = "centos"
	case "r":
		m.factoryScope = "rocky"
	case "d":
		m.factoryScope = "debian"
	case "u":
		m.factoryScope = "ubuntu"
	case "e":
		// fedora is 'e': f closes the pane and d is already debian. Spelled
		// out in the pane so nobody has to guess.
		m.factoryScope = "fedora"
	case "enter":
		if m.factoryCursor >= len(rows) {
			return m, nil
		}
		it := rows[m.factoryCursor]
		argv := m.factoryArgv(it)
		if _, err := exec.LookPath(argv[0]); err != nil {
			m.status = styWarn.Render(argv[0] + " not found on this machine")
			return m, nil
		}
		m.overlay = ""
		m.status = styCmd.Render("→ " + strings.Join(argv, " "))
		// The hint goes on the terminal the build is about to take over: a
		// golden build scrolls for tens of minutes and the status line behind
		// it is gone on the first page.
		hint := "vmxplore: running " + strings.Join(argv, " ") + " — ctrl+c aborts, you return to vmx when it ends"
		sh := append([]string{"-c", `printf '%s\n\n' "$1"; shift; exec "$@"`, "_", hint}, argv...)
		return m, tea.ExecProcess(exec.Command("/bin/sh", sh...),
			func(err error) tea.Msg { return execDoneMsg{err} })
	}
	return m, nil
}

// factoryText renders the pane.
func (m *ui) factoryText() string {
	var b strings.Builder
	scope := m.factoryScope
	if scope == "" {
		scope = "all"
	}
	fmt.Fprintf(&b, "%s%s\n", styGroup.Render("Factory"),
		styStatus.Render(" — build goldens, run suites"))
	fmt.Fprintf(&b, "%s %s   %s\n\n",
		styStatus.Render("klab scope:"), styKey.Render(scope),
		styStatus.Render("a all · c centos · r rocky · e fedora · d debian · u ubuntu"))

	rowIdx := 0
	for _, it := range factoryItems() {
		if it.section != "" {
			b.WriteString(styHeader.Render(it.section) + "\n")
			continue
		}
		cursor := "  "
		label := it.label
		if rowIdx == m.factoryCursor {
			cursor = styKey.Render("> ")
			label = styTitle.Render(label)
		}
		line := fmt.Sprintf("%s%-22s %s", cursor, label,
			styStatus.Render(strings.Join(m.factoryArgv(it), " ")))
		b.WriteString(line + "\n")
		if it.note != "" {
			b.WriteString("    " + styStatus.Render(it.note) + "\n")
		}
		rowIdx++
	}
	b.WriteString("\n" + styWarn.Render("enter runs it in this terminal — a golden build is 10–60 minutes") + "\n" +
		keyHint("enter", "run", "←/f", "close"))
	return b.String()
}
