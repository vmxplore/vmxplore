//go:build gui

// gui.go — the native vmxplore surface (Fyne), GUI-first per the operator
// call of 2026-08-07: "the frame for the VM."
//
// What it builds, in order:
//  1. The window — title EXACTLY "vmxplore": GLFW derives the WM_CLASS from
//     the title, and the shell maps WM_CLASS → vmxplore.desktop for the dock
//     icon (the zxplore lesson, verbatim).
//  2. Left pane: the estate list (grouped, filtered by the search box).
//  3. Right-top pane: the serial console — a real terminal widget attached
//     to `virsh console` for the selected domain, in-window.
//  4. Right-bottom pane: details dossier + settings + the verb buttons,
//     driving the SAME plan builders as the TUI (verbs.go): every mutation
//     shows the exact command it runs in the status line and runs it — no
//     confirmation step, including delete; all runs audit-log.
//
// Why: vmxplore is the giveaway KVM console — GUI for the desktop, TUI for
// ssh — that lights up extra powers on kldload/ZFS hosts via capability
// probes only (rules.go). The engine (libvirt.go/zfs.go/verbs.go) is shared;
// this file is presentation only.
//
// Inputs: qemu:///system via the libvirt group (NO sudo re-exec here — root
// cannot reach the user's Wayland display; zfs verbs elevate per-command in
// runPlan). Outputs: none beyond the verbs' own.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image/color"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/creack/pty"
	fyneterm "github.com/fyne-io/terminal"
)

//go:embed packaging/vmxplore.svg
var iconSVG []byte

// ── theme ──────────────────────────────────────────────────────────────────
// Ported from zxplore's compactTheme so the family shares one look: the
// near-black steel base ("the black space"), card panels lifted a step off
// it, tight list rows, and electric-on-dark / deep-on-light accents. Only
// the brand accent differs — vmxplore's compute red instead of zxplore's
// teal-green (the icon palette: red box, amber LED).

type compactTheme struct{ fyne.Theme }

func (t compactTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameInnerPadding:
		return 2 // tight rows (single-spaced list)
	case theme.SizeNamePadding:
		return 3 // space between panes / regions — small on purpose: this
		// window is mostly other machines' screens, and every margin is
		// drawn at the expense of one
	}
	return t.Theme.Size(name)
}

// tabLabel pads a tab caption so the tab bar reads as separate destinations
// rather than one run-on word.
//
// WHY IT IS SPACES AND NOT A THEME SIZE: Fyne sizes tab buttons from the
// global SizeNamePadding, which compactTheme deliberately holds at 3px — this
// window is mostly other machines' screens and every margin is drawn at the
// expense of one. Raising it to space out five tabs would loosen every list
// row, card and pane in the app to fix one strip. Padding the captions buys
// the separation exactly where it was asked for and nowhere else.
func tabLabel(s string) string { return "   " + s + "   " }

func (t compactTheme) Color(name fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	dark := v == theme.VariantDark
	switch name {
	case theme.ColorNamePrimary, theme.ColorNameHyperlink:
		if dark {
			return acBrand.dark
		}
		return acBrand.light
	case theme.ColorNameSelection:
		if dark {
			return color.NRGBA{R: 0x2a, G: 0x3a, B: 0x48, A: 0xff} // steel-blue highlight
		}
		return color.NRGBA{R: 0xec, G: 0xe2, B: 0xf8, A: 0xff} // soft lavender wash
	case theme.ColorNameBackground:
		if dark {
			return color.NRGBA{R: 0x08, G: 0x09, B: 0x0c, A: 0xff} // very dark steel-black
		}
		return color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	case theme.ColorNameInputBackground:
		if dark {
			return color.NRGBA{R: 0x13, G: 0x18, B: 0x20, A: 0xff}
		}
		return color.NRGBA{R: 0xf6, G: 0xf2, B: 0xf1, A: 0xff}
	case theme.ColorNameForeground:
		if dark {
			return color.NRGBA{R: 0xe3, G: 0xe9, B: 0xef, A: 0xff} // crisp steel-white
		}
		return color.NRGBA{R: 0x1e, G: 0x18, B: 0x18, A: 0xff}
	}
	return t.Theme.Color(name, v)
}

// accentPair: ELECTRIC on the near-black dark theme, DEEP-saturated on the
// bright light theme (the zxplore "ANSI BBS, modernized" rule).
type accentPair struct{ dark, light color.NRGBA }

func (a accentPair) at() color.Color {
	if variantDark() {
		return a.dark
	}
	return a.light
}

var (
	// The brand accent IS the details blue (operator call, 2026-08-09 —
	// the purple sat apart from everything else it was next to). One pair,
	// one name each, deliberately aliased rather than duplicated: two
	// literals that must stay equal are two literals that will not.
	//
	// It reaches further than a heading colour. Fyne resolves
	// ColorNamePrimary from it, so every HighImportance control — the
	// Storage / Configure / Build / Estate menus — follows without being
	// told, and so do the estate group names, the page headings and the
	// selected tab.
	acBlue  = accentPair{color.NRGBA{0x4d, 0xa6, 0xff, 0xff}, color.NRGBA{0x14, 0x66, 0xd8, 0xff}}
	acBrand = acBlue
	// red survives ONLY as danger — toned to the icon's compute red
	acRed   = accentPair{color.NRGBA{0xe2, 0x69, 0x5d, 0xff}, color.NRGBA{0xb8, 0x38, 0x28, 0xff}}
	acGold  = accentPair{color.NRGBA{0xff, 0xd0, 0x43, 0xff}, color.NRGBA{0xb0, 0x7d, 0x00, 0xff}} // the icon's LED amber
	acGreen = accentPair{color.NRGBA{0x3d, 0xff, 0x88, 0xff}, color.NRGBA{0x0e, 0x9d, 0x4a, 0xff}} // running
	acOff   = accentPair{color.NRGBA{0x9a, 0x7b, 0x55, 0xff}, color.NRGBA{0x7a, 0x5a, 0x38, 0xff}} // shut off — dull brown, dormant
)

// repaint holds recolor closures for hand-colored canvas objects (cards,
// headings, list rows can't follow the theme on their own); applyPalette
// re-runs them once the GNOME variant is resolved and on every theme flip —
// the zxplore fix for "dark boxes on a white app".
var repaint []func()

func applyPalette() {
	for _, f := range repaint {
		f()
	}
}

func variantDark() bool {
	return fyne.CurrentApp() != nil &&
		fyne.CurrentApp().Settings().ThemeVariant() == theme.VariantDark
}

// cardColor is the panel backdrop — lifted just off the window base.
func cardColor() color.Color {
	if variantDark() {
		return color.NRGBA{R: 0x15, G: 0x1b, B: 0x23, A: 0xff} // dark steel
	}
	return color.NRGBA{R: 0xf7, G: 0xf4, B: 0xf3, A: 0xff}
}

// card wraps a section in a rounded backdrop so the regions read as panels
// floating on the black, with the base showing through the gaps.
func card(content fyne.CanvasObject) fyne.CanvasObject {
	r := canvas.NewRectangle(cardColor())
	r.CornerRadius = 8
	repaint = append(repaint, func() { r.FillColor = cardColor(); r.Refresh() })
	return container.NewStack(r, container.NewPadded(content))
}

// cardTight is card without the inner inset — for the console, where the
// content is a guest's screen and every pixel spent on a margin is a pixel
// not spent on the machine. The rounded backdrop stays, so the pane still
// reads as a card next to the others.
func cardTight(content fyne.CanvasObject) fyne.CanvasObject {
	r := canvas.NewRectangle(cardColor())
	r.CornerRadius = 8
	repaint = append(repaint, func() { r.FillColor = cardColor(); r.Refresh() })
	return container.NewStack(r, content)
}

// heading is a bold accent-colored section title (long-lived surfaces —
// registers for theme-flip repaints).
func heading(text string, a accentPair) *canvas.Text {
	t := canvas.NewText(text, a.at())
	t.TextStyle = fyne.TextStyle{Bold: true}
	t.TextSize = 14
	repaint = append(repaint, func() { t.Color = a.at(); t.Refresh() })
	return t
}

// pageHeading is heading() for surfaces that rebuild on every visit (the
// launcher pages): same look, NO repaint registration — registering would
// leak one closure per page open (the zxplore dialogHeading rule).
func pageHeading(text string, a accentPair) *canvas.Text {
	t := canvas.NewText(text, a.at())
	t.TextStyle = fyne.TextStyle{Bold: true}
	t.TextSize = 14
	return t
}

// brightFg is the row TITLE colour — a notch brighter than the body text
// so the machine name pops: near-white on dark, near-black on light.
func brightFg() color.Color {
	if variantDark() {
		return color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	}
	return color.NRGBA{R: 0x0a, G: 0x0a, B: 0x0a, A: 0xff}
}

// rowDetail is the faint second line of an estate row: specs · IP ·
// group · zvol facts — everything a glance should answer without opening
// the dossier. Only present fields render, joined by " · ".
func rowDetail(r Row, group string) string {
	var parts []string
	if r.D.VCPUs > 0 {
		parts = append(parts, fmt.Sprintf("%dvcpu", r.D.VCPUs))
	}
	if mem := memCell(r); mem != "" && mem != "-" {
		parts = append(parts, mem)
	}
	if ip := firstIPv4(r.D.IPs); ip != "" {
		parts = append(parts, ip)
	} else if r.D.State == "running" && !r.D.AgentUp {
		parts = append(parts, "no-agent")
	}
	if group != "" {
		parts = append(parts, group)
	}
	if r.DS != nil {
		z := humanBytes(r.DS.Used)
		if r.SnapTotal > 0 {
			z += fmt.Sprintf(" ×%d", r.SnapTotal)
		}
		if r.Origin != "" {
			z += " ⑂" // a clone — forked from a parent
		}
		parts = append(parts, z)
	}
	if len(r.Notes) > 0 {
		parts = append(parts, strings.Join(r.Notes, "; "))
	}
	return "   " + strings.Join(parts, " · ")
}

// tileColor lifts a launcher tile one more step off the card backdrop.
func tileColor() color.Color {
	if variantDark() {
		return color.NRGBA{R: 0x1c, G: 0x24, B: 0x30, A: 0xff}
	}
	return color.NRGBA{R: 0xee, G: 0xe9, B: 0xe7, A: 0xff}
}

// tileSubColor is the tile's secondary text, picked by the SAME variantDark()
// call as tileColor().
//
// WHY IT IS NOT A THEME LABEL: the description used to be a widget.Label,
// which takes its colour from Fyne's theme with the LIVE variant, while the
// rectangle behind it took tileColor() from Settings().ThemeVariant(). When
// those two disagreed the tile drew dark text on a dark card and the
// description became invisible in light mode (operator screenshot,
// 2026-08-18). Deriving both from one call makes disagreement impossible
// rather than unlikely.
func tileSubColor() color.Color {
	if variantDark() {
		return color.NRGBA{R: 0x9a, G: 0xa4, B: 0xb2, A: 0xff}
	}
	return color.NRGBA{R: 0x5a, G: 0x62, B: 0x6f, A: 0xff}
}

// ellipsise trims to n runes and adds an ellipsis, so a tile stays one line
// without needing widget.Label's truncation — see tileSubColor for why the
// widget went away. Rune-safe: cutting bytes would split a multi-byte glyph.
func ellipsise(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

// tapArea makes any canvas object tappable with a pointer cursor — the
// chassis under the launcher tiles (buttons can't hold two-line content).
type tapArea struct {
	widget.BaseWidget
	content fyne.CanvasObject
	onTap   func()
}

func newTapArea(content fyne.CanvasObject, onTap func()) *tapArea {
	t := &tapArea{content: content, onTap: onTap}
	t.ExtendBaseWidget(t)
	return t
}

func (t *tapArea) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(t.content)
}
func (t *tapArea) Tapped(*fyne.PointEvent) { t.onTap() }
func (t *tapArea) Cursor() desktop.Cursor  { return desktop.PointerCursor }

// hoverReveal shows its content only while the pointer is inside it.
//
// Fullscreen has exactly one way out (see the ⛶ comment) and that control
// has to stay reachable — but a button parked over the guest covers guest
// pixels for an entire session in order to be useful for the single click
// that ends it. A guest that draws anything in its top-right corner (a menu,
// a close button, a tray) loses it behind an icon. This keeps a small hot
// corner that is transparent until the pointer arrives.
//
// The area must exist to be hovered at all, hence a transparent rectangle
// sized to the content rather than an empty container: a zero-size widget
// receives no mouse events, and a widget that grows when the content appears
// would move the control out from under the pointer that just summoned it.
type hoverReveal struct {
	widget.BaseWidget
	content fyne.CanvasObject
}

func newHoverReveal(content fyne.CanvasObject) *hoverReveal {
	h := &hoverReveal{content: content}
	content.Hide()
	h.ExtendBaseWidget(h)
	return h
}

func (h *hoverReveal) CreateRenderer() fyne.WidgetRenderer {
	hot := canvas.NewRectangle(color.Transparent)
	hot.SetMinSize(h.content.MinSize())
	return widget.NewSimpleRenderer(container.NewStack(hot, h.content))
}

func (h *hoverReveal) MouseIn(*desktop.MouseEvent)    { h.content.Show() }
func (h *hoverReveal) MouseMoved(*desktop.MouseEvent) {}
func (h *hoverReveal) MouseOut()                      { h.content.Hide() }

// vmRow is one estate list row: two lines — a bold state-coloured title
// (dot · name · state) over a faint detail line (specs · IP · group · zvol)
// so the left pane reads as a dossier, not a bare name column. Selects on
// tap, opens the verb context menu on right-click.
type vmRow struct {
	widget.BaseWidget
	title    *canvas.Text
	detail   *canvas.Text
	onTap    func() // click the row body → select (drives panes)
	onToggle func() // click the state dot → toggle batch check
	onRange  func() // ctrl/shift-click → check the range from the anchor
	onMenu   func(pos fyne.Position)
	modDown  bool // Ctrl or Shift held on MouseDown, read by the next Tapped

	// The dot zone is a 22px target that does something completely different
	// from the other ~95% of the row, and nothing was drawn to say so. The
	// author of the feature could not find it (2026-08-26) — which is the
	// clearest possible evidence that an invisible affordance is not one.
	// zoneHL is a faint rectangle shown only while the pointer is inside the
	// zone, so the batch target announces itself on approach instead of
	// having to be known about in advance.
	zoneHL *canvas.Rectangle
	inZone bool
}

// dotZoneW is how wide (px) the leading state-dot hit zone is: clicking
// inside it toggles the batch checkbox, outside it selects the row.
const dotZoneW = 22

// MouseDown records whether Ctrl or Shift was held, so the paired Tapped
// can tell a range-select click from a plain one (Fyne delivers MouseDown
// before Tapped for the same press).
func (r *vmRow) MouseDown(e *desktop.MouseEvent) {
	r.modDown = e.Modifier&(fyne.KeyModifierControl|fyne.KeyModifierShift) != 0
}
func (r *vmRow) MouseUp(*desktop.MouseEvent) {}

// Tree UIDs for the catalog branch. They live outside the "grp/" and "vm/"
// namespaces so no estate lookup can ever collide with a catalog entry.
const (
	applianceBranchUID = "appliances"
	applianceUIDPrefix = "app/"
	selfTestUID        = "app-selftest"
	buildAllUID        = "app-build-all"
	destroyAllUID      = "app-destroy-all"
	// The Firecracker branch: a fixture whenever kfire is on the host —
	// the Clone row, then the goldens, then the instances (as "vm/" rows,
	// so they carry the estate verbs). Outside "grp/" for the same reason
	// the catalog is: it is not a libvirt group.
	fcBranchUID       = "firecracker"
	fcGoldenUIDPrefix = "fcg/"
	fcCloneUID        = "fc-clone"       // clone a golden N times
	fcMakeGoldenUID   = "fc-make-golden" // pick a shut-off appliance → kfire golden
	fcDestroyAllUID   = "fc-destroy-all" // kfire destroy --all, confirmed
	// The kldload tool launcher, also a tree branch: one sub-branch per
	// tool group, one row per tool. On a host without the toolset the branch
	// holds a single row that says where to get it.
	toolsBranchUID     = "tools"
	toolGroupUIDPrefix = "tools/"
	toolUIDPrefix      = "tool/"
	getKldloadUID      = "tool-get-kldload"
)

func newVMRow() *vmRow {
	r := &vmRow{
		title:  canvas.NewText("", theme.Color(theme.ColorNameForeground)),
		detail: canvas.NewText("", theme.Color(theme.ColorNameForeground)),
	}
	r.title.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
	r.detail.TextStyle = fyne.TextStyle{Monospace: true}
	r.detail.TextSize = theme.TextSize() * 0.85
	r.ExtendBaseWidget(r)
	return r
}

func (r *vmRow) CreateRenderer() fyne.WidgetRenderer {
	// tight two-line stack; the detail sits just under the title
	if r.zoneHL == nil {
		r.zoneHL = canvas.NewRectangle(theme.Color(theme.ColorNameHover))
		r.zoneHL.CornerRadius = 3
		r.zoneHL.Hide()
	}
	box := container.New(&rowLayout{}, r.title, r.detail)
	// The highlight is laid out by dotLayout so it covers exactly the hit
	// zone Tapped tests against — one constant, dotZoneW, drives both.
	return widget.NewSimpleRenderer(container.New(&dotOverlay{}, r.zoneHL, box))
}

// dotOverlay puts the zone highlight under the row content, sized to the same
// dotZoneW that Tapped uses. Deriving both from one constant means the visible
// target and the clickable target cannot drift apart.
type dotOverlay struct{}

func (dotOverlay) MinSize(o []fyne.CanvasObject) fyne.Size { return o[1].MinSize() }
func (dotOverlay) Layout(o []fyne.CanvasObject, sz fyne.Size) {
	o[0].Move(fyne.NewPos(0, 0))
	o[0].Resize(fyne.NewSize(dotZoneW, sz.Height))
	o[1].Move(fyne.NewPos(0, 0))
	o[1].Resize(sz)
}

// Hoverable: reveal the batch target when the pointer enters it.
func (r *vmRow) MouseIn(e *desktop.MouseEvent) { r.MouseMoved(e) }
func (r *vmRow) MouseMoved(e *desktop.MouseEvent) {
	in := e.Position.X < dotZoneW
	if in == r.inZone {
		return // no state change: do not repaint on every mouse move
	}
	r.inZone = in
	if r.zoneHL == nil {
		return
	}
	if in {
		r.zoneHL.Show()
	} else {
		r.zoneHL.Hide()
	}
	r.zoneHL.Refresh()
}
func (r *vmRow) MouseOut() {
	r.inZone = false
	if r.zoneHL != nil {
		r.zoneHL.Hide()
		r.zoneHL.Refresh()
	}
}

// Cursorable: a pointer cursor over the zone, the default elsewhere — the
// second half of saying "this part is a different control".
func (r *vmRow) Cursor() desktop.Cursor {
	if r.inZone {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

// rowLayout stacks the title and detail with a small gap, sizing to both —
// VBox padding was too airy for a dense list.
type rowLayout struct{}

func (rowLayout) MinSize(o []fyne.CanvasObject) fyne.Size {
	t, d := o[0].MinSize(), o[1].MinSize()
	w := t.Width
	if d.Width > w {
		w = d.Width
	}
	return fyne.NewSize(w, t.Height+d.Height+2)
}

func (rowLayout) Layout(o []fyne.CanvasObject, sz fyne.Size) {
	t := o[0].MinSize()
	o[0].Move(fyne.NewPos(0, 0))
	o[0].Resize(fyne.NewSize(sz.Width, t.Height))
	o[1].Move(fyne.NewPos(0, t.Height+2))
	o[1].Resize(fyne.NewSize(sz.Width, o[1].MinSize().Height))
}

func (r *vmRow) Tapped(e *fyne.PointEvent) {
	// ctrl/shift-click anywhere → range-check from the anchor to here
	if r.modDown && r.onRange != nil {
		r.onRange()
		return
	}
	// the leading dot is the batch checkbox; the rest of the row selects
	if e.Position.X < dotZoneW && r.onToggle != nil {
		r.onToggle()
		return
	}
	if r.onTap != nil {
		r.onTap()
	}
}

func (r *vmRow) TappedSecondary(e *fyne.PointEvent) {
	if r.onMenu != nil {
		r.onMenu(e.AbsolutePosition)
	}
}

// newTile is one launcher card: icon + bold coloured title + a faint
// wrapped description — big enough that a screenshot of the grid explains
// the product on its own (the operator's ask, verbatim).
// tileSize is THE tile footprint. Every grid in the app uses it, so a tile
// is the same object wherever it appears and the tabs stop looking subtly
// unlike each other. 220 is the compromise the three former sizes were
// circling: wide enough for the longest tool label, narrow enough that a
// section of six fits one row on a 1080p window instead of wrapping into
// the next heading.
var tileSize = fyne.NewSize(220, 92)

func newTile(icon fyne.Resource, title, desc string, col color.Color, onTap func()) fyne.CanvasObject {
	bg := canvas.NewRectangle(tileColor())
	bg.CornerRadius = 8
	tt := canvas.NewText(title, col)
	tt.TextStyle = fyne.TextStyle{Bold: true}
	tt.TextSize = 15
	// Truncate rather than wrap. Wrapping made every tile a different
	// height inside a fixed grid cell: a short description left half the
	// tile empty and a long one pushed past the bottom edge, so a grid of
	// them looked ragged however carefully the cell was sized. One line,
	// ellipsised, means every tile is the same object — which is the whole
	// point of a tile. The full text still arrives on hover.
	// Same one-line-per-tile rule as before; see ellipsise/tileSubColor.
	d := canvas.NewText(ellipsise(desc, 34), tileSubColor())
	d.TextSize = 12
	content := container.NewVBox(
		container.NewHBox(widget.NewIcon(icon), tt), d)
	return newTapArea(container.NewStack(bg, container.NewPadded(content)), onTap)
}

// guardConfirm reports whether the form is good enough to submit. When it
// is not it shows why and re-opens the dialog with everything still typed.
func guardConfirm(err error, redo func(), w fyne.Window) bool {
	if err == nil {
		return true
	}
	dialog.ShowError(err, w)
	redo()
	return false
}

// guiState is everything the refresh goroutine and the widgets share. All
// mutation happens inside fyne.Do — the ticker computes off-thread and
// applies on-thread, so there is no lock.
type guiState struct {
	lv      *LV
	rs      *Ruleset
	groups  []GroupRows
	rows    []Row // flat, filtered — what the list renders
	filter  string
	dss     map[string]*Dataset
	snaps   map[string][]string
	ann     *Annotations
	prevCPU map[string]uint64
	prevAt  time.Time
	cpu     map[string]float64
	selName string // selection survives refresh by domain name

	// console plumbing: the exec'd virsh console + its pty, killed on
	// re-attach, detach and window close so no console outlives its pane
	conCmd  *exec.Cmd
	conPty  interface{ Close() error }
	conName string
}

// visibleRows flattens groups → rows honouring the search filter.
func (g *guiState) visibleRows() []Row {
	var out []Row
	q := strings.ToLower(strings.TrimSpace(g.filter))
	for _, gr := range g.groups {
		for _, r := range gr.Rows {
			if q == "" || strings.Contains(strings.ToLower(r.D.Name), q) {
				out = append(out, r)
			}
		}
	}
	return out
}

func (g *guiState) selected() (Row, bool) {
	for _, r := range g.rows {
		if r.D.Name == g.selName {
			return r, true
		}
	}
	return Row{}, false
}

// dossierSegs renders the details pane — the GUI twin of the TUI's
// detailText, same colour language via theme colours: accent labels,
// green running, warnings amber. RichText named colours follow the theme
// variant for free.
func (g *guiState) dossierSegs(r Row) []widget.RichTextSegment {
	mono := widget.RichTextStyle{
		Inline: true, TextStyle: fyne.TextStyle{Monospace: true}}
	title := mono
	title.TextStyle.Bold = true
	title.ColorName = theme.ColorNamePrimary
	label := mono
	label.ColorName = theme.ColorNamePrimary
	good := mono
	good.ColorName = theme.ColorNameSuccess
	warn := mono
	warn.ColorName = theme.ColorNameWarning
	segs := []widget.RichTextSegment{
		&widget.TextSegment{Text: r.D.Name + "\n\n", Style: title}}
	add := func(k string, sty widget.RichTextStyle, v string) {
		segs = append(segs,
			&widget.TextSegment{Text: fmt.Sprintf("%-9s ", k), Style: label},
			&widget.TextSegment{Text: v + "\n", Style: sty})
	}
	stateSty := mono
	if r.D.State == "running" {
		stateSty = good
	}
	add("state", stateSty, r.D.State)
	if r.FC != nil {
		add("runtime", mono, "Firecracker microVM · golden "+r.FC.Golden)
		add("vcpu/mem", mono, fmt.Sprintf("%d / %d MB ceiling", r.FC.VCPUs, r.FC.RAMMB))
		add("disk", mono, "vda → /dev/zvol/"+r.FC.RootZvol)
		if r.FC.DataZvol != "" {
			add("disk", mono, "vdb → /dev/zvol/"+r.FC.DataZvol)
		}
		add("net", mono, r.FC.Tap+" on "+r.FC.Bridge+" · "+r.FC.MAC)
		if r.FC.IP != "" {
			add("ip", mono, r.FC.IP+fmt.Sprintf("  (http://%s:%d/)", r.FC.IP, r.FC.Port))
		}
		add("unit", mono, "kfire-"+r.FC.Name+" · kfire console "+r.FC.Name)
	}
	if !r.Synthetic && r.FC == nil {
		add("uuid", mono, r.D.UUID)
		add("vcpu/mem", mono, fmt.Sprintf("%d / %s", r.D.VCPUs, memCell(r)))
		auto := "no"
		if r.D.Autostart {
			auto = "yes"
		}
		if r.D.Persistent {
			add("config", mono, "autostart "+auto+" · persistent")
		} else {
			add("config", warn, "autostart "+auto+" · TRANSIENT — gone after destroy")
		}
		for _, d := range r.D.Disks {
			add("disk", mono, d.Target+" → "+cellOr(d.Dev, d.File))
		}
		if len(r.D.IPs) > 0 {
			add("ip", mono, strings.Join(r.D.IPs, ", "))
		}
	}
	if r.DS != nil {
		add("dataset", mono, fmt.Sprintf("%s  (used %s, refer %s)",
			r.DS.Name, humanBytes(r.DS.Used), humanBytes(r.DS.Refer)))
		if chain := OriginChain(r.DS, g.dss); len(chain) > 0 {
			add("lineage", mono, strings.Join(chain, " ← "))
		}
		if all := g.snaps[r.DS.Name]; len(all) > 0 {
			byClass := map[string]int{}
			for _, s := range all {
				byClass[g.rs.ClassifySnap(s)]++
			}
			classes := make([]string, 0, len(byClass))
			for c := range byClass {
				classes = append(classes, c)
			}
			sort.Strings(classes)
			parts := make([]string, 0, len(classes))
			for _, c := range classes {
				parts = append(parts, fmt.Sprintf("%d %s", byClass[c], c))
			}
			add("snaps", mono, fmt.Sprintf("%d  (%s)",
				len(all), strings.Join(parts, " · ")))
		}
	}
	for _, n := range r.Notes {
		add("note", warn, n)
	}
	return segs
}

// guiStatus, if set, gets a one-line note when a verb fires — the exact
// command still surfaces (teach-the-CLI), in the status bar rather than a
// modal. Set once in runGUI.
var guiStatus func(string)

// pickPubKey returns the operator's ssh public key for prefilling the
// guest-login field, or "" when there is none to offer. It walks a
// preference order and then falls back to any *.pub, because hardcoding
// one filename silently offered nothing on a host whose only key is an
// id_kldload or an id_rsa — and an empty key box next to an empty password
// box is how a VM gets built that nobody can log into.
func pickPubKey() string {
	dir := os.Getenv("HOME") + "/.ssh/"
	for _, n := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		if b, err := os.ReadFile(dir + n); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	names, _ := filepath.Glob(dir + "*.pub")
	for _, n := range names {
		if b, err := os.ReadFile(n); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// firePlan runs a plan and reports it. There is no confirmation step of any
// kind, including for delete.
//
// WHY (operator call, 2026-08-09): these are cattle, not pets. A VM here is
// a thing you make in seconds from a golden and remake just as fast, and
// every dialog between the click and the deed was taxing the common case to
// insure against a rare one. Clicking delete IS the confirmation.
//
// What remains of the safety net is deliberate and worth knowing: every
// command lands in the audit log (runPlan → /var/log/kldload/vmx.log) with
// who ran it and its exit code, and the command itself shows in the status
// bar as it fires. WARN: `zfs destroy -r` takes the dataset's snapshots
// with it, so sanoid history on that zvol is not a recovery path — only
// replication to another pool or host is.
func firePlan(w fyne.Window, p verbPlan, after func()) {
	if guiStatus != nil {
		note := "· " + strings.TrimSpace(strings.TrimPrefix(
			strings.TrimSpace(p.cmdLines()), "$"))
		if p.warn != "" {
			note += "  ⚠ " + p.warn
		}
		guiStatus(note)
	}
	go func() {
		err := runPlan(p)
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(err, w)
			}
			after()
		})
	}()
}

func runGUI(rs *Ruleset) {
	a := app.NewWithID("dev.vmxplore")
	a.Settings().SetTheme(compactTheme{theme.DefaultTheme()})
	a.SetIcon(fyne.NewStaticResource("vmxplore.svg", iconSVG))
	// Title EXACTLY "vmxplore" — WM_CLASS ← title ← dock icon (see banner).
	w := a.NewWindow("vmxplore")
	w.Resize(fyne.NewSize(1600, 1000))
	w.CenterOnScreen()

	lv, err := ConnectSystem()
	if err != nil {
		// no estate, no app — one clear dialog then exit; sudo can't help a
		// GUI (Wayland), the fix is the libvirt group
		errLabel := widget.NewLabel(fmt.Sprintf(
			"libvirt: %v\n\nqemu:///system needs membership in the libvirt\n"+
				"group (log out/in after adding) — or run `vmx --tui` under sudo.", err))
		w.SetContent(container.NewCenter(errLabel))
		w.ShowAndRun()
		return
	}

	st := &guiState{lv: lv, rs: rs, cpu: map[string]float64{}}
	// checked = the batch set (click a row's state dot to toggle). Batch
	// verbs act on every checked VM at once. refreshBatchBar is wired below.
	checked := map[string]bool{}
	checkAnchor := "" // last dot-toggled VM; ctrl/shift-click ranges from it
	var refreshBatchBar func()

	// ── left: the estate tree ────────────────────────────────────────────
	// Groups are collapsible branches (the accordion), VMs are two-line
	// leaves. viewGroups is st.groups after the search filter; the tree and
	// st.rows both read it so selection, filtering and the flat helpers
	// stay in sync. uid scheme: "grp/<label>" branch, "vm/<name>" leaf.
	var viewGroups []GroupRows
	// fcRowsNow is the Firecracker instances as rows: in st.rows so
	// selection, verbs and the batch bar see them, under their own branch
	// in the tree rather than a "grp/" group.
	var fcRowsNow []Row
	rebuildView := func() {
		q := strings.ToLower(strings.TrimSpace(st.filter))
		viewGroups = viewGroups[:0]
		st.rows = st.rows[:0]
		for _, g := range st.groups {
			var rows []Row
			for _, r := range g.Rows {
				if q == "" || strings.Contains(strings.ToLower(r.D.Name), q) {
					rows = append(rows, r)
				}
			}
			if len(rows) > 0 {
				viewGroups = append(viewGroups, GroupRows{Label: g.Label, Rows: rows})
				st.rows = append(st.rows, rows...)
			}
		}
		for _, r := range fcRowsNow {
			if q == "" || strings.Contains(strings.ToLower(r.D.Name), q) {
				st.rows = append(st.rows, r)
			}
		}
	}
	rowByUID := func(uid string) (Row, bool) {
		name := strings.TrimPrefix(uid, "vm/")
		for _, g := range viewGroups {
			for _, r := range g.Rows {
				if r.D.Name == name {
					return r, true
				}
			}
		}
		for _, r := range fcRowsNow {
			if r.D.Name == name {
				return r, true
			}
		}
		return Row{}, false
	}
	groupStats := func(label string) (n, run int) {
		for _, g := range viewGroups {
			if g.Label == label {
				n = len(g.Rows)
				for _, r := range g.Rows {
					if r.D.State == "running" {
						run++
					}
				}
			}
		}
		return
	}

	var tree *widget.Tree
	var rowMenuAt func(r Row, pos fyne.Position) // wired after the verbs exist
	// openAppliance is wired once the dialog exists, further down. The tree
	// is built before it, so the catalog branch reaches it through this.
	var openAppliance func(name string)
	var openSelfTest func()
	var openBuildAll func()
	// buildAllStatus is what the Build all tile itself says while a run is
	// going and after it ends — the acknowledgement at the point of the
	// click. The log window can open behind a fullscreen main window, and
	// "I press Build all and nothing happens" was said twice in one evening
	// (onyx, 2026-09-04); the row the operator is looking at now changes.
	var buildAllStatus string
	var openDestroyAll func()
	var openFCClone func()
	var openFCCloneFor func(golden string)
	var openFCMakeGolden, openFCDestroyAll func()
	// openTool is wired once the tools pane exists (it needs the pty host);
	// the groups are probed once because the tree repaints constantly and
	// each probe is a LookPath per tool.
	var openTool func(tool string)
	toolGroups := KldloadToolGroups()
	childUIDs := func(uid string) []string {
		if uid == "" {
			out := make([]string, 0, len(viewGroups)+2)
			for _, g := range viewGroups {
				out = append(out, "grp/"+g.Label)
			}
			// The catalog and the toolset sit last and closed: they are
			// menus of things to build or run, not estate, so they must
			// never push the running VMs down the pane. They used to be
			// tabs in the console card as well; listing the same things
			// twice was the redundancy the operator asked to lose
			// (2026-09-03), so the console keeps Serial and Screen only.
			if kfireAvailable() {
				out = append(out, fcBranchUID)
			}
			return append(out, applianceBranchUID, toolsBranchUID)
		}
		if uid == fcBranchUID {
			gs := fcGoldensCached()
			out := make([]string, 0, len(gs)+len(fcRowsNow)+3)
			out = append(out, fcMakeGoldenUID, fcCloneUID)
			for _, g := range gs {
				out = append(out, fcGoldenUIDPrefix+g.Name)
			}
			for _, r := range fcRowsNow {
				out = append(out, "vm/"+r.D.Name)
			}
			if len(fcRowsNow) > 0 {
				out = append(out, fcDestroyAllUID)
			}
			return out
		}
		if uid == toolsBranchUID {
			if len(toolGroups) == 0 {
				return []string{getKldloadUID}
			}
			out := make([]string, 0, len(toolGroups)+1)
			for _, g := range toolGroups {
				out = append(out, toolGroupUIDPrefix+g.Name)
			}
			// The plain prompt is not one of the tools and gets its own
			// footer group, so it never sits inside a category it does not
			// belong to.
			return append(out, toolGroupUIDPrefix+"Shell")
		}
		if name, ok := strings.CutPrefix(uid, toolGroupUIDPrefix); ok {
			if name == "Shell" {
				return []string{toolUIDPrefix + "shell"}
			}
			for _, g := range toolGroups {
				if g.Name == name {
					out := make([]string, 0, len(g.Tools))
					for _, t := range g.Tools {
						out = append(out, toolUIDPrefix+t)
					}
					return out
				}
			}
			return nil
		}
		if uid == applianceBranchUID {
			out := make([]string, 0, len(Appliances())+3)
			out = append(out, selfTestUID, buildAllUID, destroyAllUID)
			for _, n := range ApplianceNames() {
				out = append(out, applianceUIDPrefix+n)
			}
			return out
		}
		if label, ok := strings.CutPrefix(uid, "grp/"); ok {
			for _, g := range viewGroups {
				if g.Label == label {
					out := make([]string, 0, len(g.Rows))
					for _, r := range g.Rows {
						out = append(out, "vm/"+r.D.Name)
					}
					return out
				}
			}
		}
		return nil
	}
	isBranch := func(uid string) bool {
		return uid == "" || uid == applianceBranchUID || uid == toolsBranchUID || uid == fcBranchUID ||
			strings.HasPrefix(uid, "grp/") || strings.HasPrefix(uid, toolGroupUIDPrefix)
	}
	tree = widget.NewTree(childUIDs, isBranch,
		func(branch bool) fyne.CanvasObject {
			if branch {
				t := canvas.NewText("", acBrand.at())
				t.TextStyle = fyne.TextStyle{Bold: true}
				return t
			}
			return newVMRow()
		},
		func(uid string, branch bool, o fyne.CanvasObject) {
			if branch {
				t := o.(*canvas.Text)
				if uid == applianceBranchUID {
					t.Text = fmt.Sprintf("Apps  (%d)", len(Appliances()))
					t.Color = acBrand.at()
					t.Refresh()
					return
				}
				if uid == fcBranchUID {
					run := 0
					for _, r := range fcRowsNow {
						if r.D.State == "running" {
							run++
						}
					}
					t.Text = fmt.Sprintf("Firecracker  (%d golden, %d microVM, %d running)", len(fcGoldensCached()), len(fcRowsNow), run)
					t.Color = acBrand.at()
					t.Refresh()
					return
				}
				if uid == toolsBranchUID {
					n := 0
					for _, g := range toolGroups {
						n += len(g.Tools)
					}
					t.Text = "kldload"
					if n > 0 {
						t.Text = fmt.Sprintf("kldload tools  (%d)", n)
					}
					t.Color = acBrand.at()
					t.Refresh()
					return
				}
				if name, ok := strings.CutPrefix(uid, toolGroupUIDPrefix); ok {
					t.Text = name
					t.Color = acBrand.at()
					t.Refresh()
					return
				}
				label := strings.TrimPrefix(uid, "grp/")
				n, run := groupStats(label)
				s := fmt.Sprintf("%s  (%d)", label, n)
				if run > 0 {
					s += fmt.Sprintf("  ·  %d running", run)
				}
				t.Text = s
				t.Color = acBrand.at()
				t.Refresh()
				return
			}
			// A catalog leaf is a thing to build, not a thing that exists,
			// so it borrows the row widget but none of its estate gestures.
			// Every callback is reassigned because Fyne recycles leaf
			// widgets — a stale closure here would aim a VM verb at an
			// appliance.
			if g, ok := strings.CutPrefix(uid, fcGoldenUIDPrefix); ok {
				// A golden: what a clone clones. The row is the shortcut to
				// cloning it; the sizes are what a clone inherits.
				row := o.(*vmRow)
				row.title.Text = "◇ " + g
				row.title.Color = acBrand.at()
				row.detail.Text = "   golden"
				for _, fg := range fcGoldensCached() {
					if fg.Name == g {
						data := ""
						if fg.DataZvol != "" {
							data = " + data pool"
						}
						row.detail.Text = fmt.Sprintf("   golden · %d vCPU / %d MB%s · %d clone(s) · click to clone",
							fg.VCPUs, fg.RAMMB, data, fg.Clones)
					}
				}
				row.detail.Color = theme.Color(theme.ColorNameForeground)
				row.onTap = func() { openFCCloneFor(g) }
				row.onToggle = func() {}
				row.onRange = func() {}
				row.onMenu = func(fyne.Position) {}
				row.Refresh()
				return
			}
			if uid == selfTestUID || uid == buildAllUID || uid == destroyAllUID || uid == fcCloneUID ||
				uid == fcMakeGoldenUID || uid == fcDestroyAllUID {
				// The three catalog-wide verbs. `open` is a pointer because
				// the window closures are assigned after the tree exists.
				row := o.(*vmRow)
				var open *func()
				switch uid {
				case selfTestUID:
					row.title.Text = "▶ Self-test"
					row.title.Color = acBrand.at()
					row.detail.Text = "build and audit every tile — the proof, not the promise"
					open = &openSelfTest
				case buildAllUID:
					row.title.Text = "▶ Build all"
					row.title.Color = acBrand.at()
					row.detail.Text = "one of everything, kept and shut off — tiles that already exist are skipped"
					if buildAllStatus != "" {
						row.detail.Text = buildAllStatus
					}
					open = &openBuildAll
				case fcMakeGoldenUID:
					row.title.Text = "◆ Make a golden…"
					row.title.Color = acBrand.at()
					row.detail.Text = "snapshot a shut-off appliance VM's zvols and pull its kernel — what clones clone"
					open = &openFCMakeGolden
				case fcDestroyAllUID:
					row.title.Text = "✕ Destroy all microVMs"
					row.title.Color = acGold.at()
					row.detail.Text = "kfire destroy --all: every instance with its zvols, tap, seed, unit and estate row"
					open = &openFCDestroyAll
				case fcCloneUID:
					row.title.Text = "⚡ Clone microVMs"
					row.title.Color = acBrand.at()
					row.detail.Text = "Firecracker clones of a golden — 250 ms each, serving in seconds; they appear under \"firecracker\""
					open = &openFCClone
				default:
					row.title.Text = "✕ Destroy all"
					row.title.Color = acGold.at()
					row.detail.Text = "remove every VM this catalog built (app-* and st-*), nothing else"
					open = &openDestroyAll
				}
				row.detail.Color = theme.Color(theme.ColorNameForeground)
				row.onTap = func() {
					if *open != nil {
						(*open)()
					}
				}
				row.onToggle = func() {}
				row.onRange = func() {}
				row.onMenu = func(fyne.Position) {}
				row.title.Refresh()
				row.detail.Refresh()
				return
			}
			if uid == getKldloadUID {
				// the promotion surface on generic hosts — tier 3, sold not faked
				row := o.(*vmRow)
				row.title.Text = "⇗ Get kldload"
				row.title.Color = acGold.at()
				row.detail.Text = "kldload hosts grow a tool launcher here — clusters, goldens, demos, one click"
				row.detail.Color = theme.Color(theme.ColorNameForeground)
				row.onTap = func() {
					u, _ := url.Parse("https://kldload.com")
					_ = a.OpenURL(u)
				}
				row.onToggle = func() {}
				row.onRange = func() {}
				row.onMenu = func(fyne.Position) {}
				row.title.Refresh()
				row.detail.Refresh()
				return
			}
			if name, ok := strings.CutPrefix(uid, toolUIDPrefix); ok {
				// A tool row: the colour says what it does before you read
				// it, same language the verb page uses one level down.
				row := o.(*vmRow)
				row.title.Text = "▸ " + name
				row.title.Color = toolAccent(name).at()
				row.detail.Text = toolDesc[name]
				row.detail.Color = theme.Color(theme.ColorNameForeground)
				row.onTap = func() {
					if openTool != nil {
						openTool(name)
					}
				}
				row.onToggle = func() {}
				row.onRange = func() {}
				row.onMenu = func(fyne.Position) {}
				row.title.Refresh()
				row.detail.Refresh()
				return
			}
			if name, ok := strings.CutPrefix(uid, applianceUIDPrefix); ok {
				a, found := ApplianceByName(name)
				if !found {
					return
				}
				row := o.(*vmRow)
				row.title.Text = "＋ " + a.Name
				// The colour IS the availability verdict for THIS host:
				// green builds fully, gold degrades (the detail says how),
				// dull cannot build here at all.
				level, blurb := ApplianceFit(a)
				switch level {
				case "degraded":
					row.title.Color = acGold.at()
				case "unavailable":
					row.title.Color = acOff.at()
				default:
					row.title.Color = acGreen.at()
				}
				row.detail.Text = fmt.Sprintf("%s   ·   %d vCPU, %d MB, %d GB",
					a.Summary, a.VCPUs, a.RAMMB, a.DiskGB)
				if blurb != "" {
					row.detail.Text += "   ·   " + blurb
				}
				if level == "degraded" {
					row.detail.Color = acGold.at()
				} else {
					row.detail.Color = theme.Color(theme.ColorNameForeground)
				}
				row.onTap = func() {
					if openAppliance != nil {
						openAppliance(a.Name)
					}
				}
				row.onToggle = func() {}
				row.onRange = func() {}
				row.onMenu = func(fyne.Position) {}
				row.title.Refresh()
				row.detail.Refresh()
				return
			}
			r, ok := rowByUID(uid)
			if !ok {
				return
			}
			// title colour is the state at a glance: green running, dull
			// brown shut-off (dormant), amber warned, bright-white for the
			// in-between states (paused, shutting down); the stats line
			// sits at full foreground, one notch below the title
			dot, col := "◆", brightFg()
			switch {
			case r.Synthetic || len(r.Notes) > 0:
				dot, col = "!", acGold.at()
			case r.D.State == "running":
				dot, col = "●", acGreen.at()
			case r.D.State == "shut off":
				dot, col = "○", acOff.at()
			}
			// batch-checked → the dot becomes a filled check in the brand
			// colour, so the selection is unmistakable
			if checked[r.D.Name] {
				dot, col = "☑", acBrand.at()
			}
			cpu := ""
			if c, ok := st.cpu[r.D.Name]; ok && r.D.State == "running" {
				cpu = fmt.Sprintf("   %.0f%% cpu", c)
			}
			row := o.(*vmRow)
			row.title.Text = fmt.Sprintf("%s %s   %s%s",
				dot, r.D.Name, r.D.State, cpu)
			row.title.Color = col
			row.detail.Text = rowDetail(r, "") // group is the branch above
			row.detail.Color = theme.Color(theme.ColorNameForeground)
			// the leaf widget consumes taps, so it must drive selection
			// itself — the tree never sees the click otherwise
			leafUID := uid
			name := r.D.Name
			row.onTap = func() { tree.Select(leafUID) }
			row.onToggle = func() {
				if checked[name] {
					delete(checked, name)
				} else {
					checked[name] = true
				}
				checkAnchor = name // range select measures from here
				tree.RefreshItem(leafUID)
				if refreshBatchBar != nil {
					refreshBatchBar()
				}
			}
			// ctrl/shift-click → check every VM between the anchor and
			// this one, in visible order (the file-manager range gesture)
			row.onRange = func() {
				if checkAnchor == "" {
					row.onToggle()
					return
				}
				lo, hi := -1, -1
				for i, rr := range st.rows {
					if rr.D.Name == checkAnchor {
						lo = i
					}
					if rr.D.Name == name {
						hi = i
					}
				}
				if lo < 0 || hi < 0 {
					return
				}
				if lo > hi {
					lo, hi = hi, lo
				}
				for i := lo; i <= hi; i++ {
					checked[st.rows[i].D.Name] = true
				}
				tree.Refresh()
				if refreshBatchBar != nil {
					refreshBatchBar()
				}
			}
			row.onMenu = func(pos fyne.Position) {
				if rowMenuAt != nil {
					rowMenuAt(r, pos)
				}
			}
			row.title.Refresh()
			row.detail.Refresh()
		},
	)

	// The fullscreen pair is declared here and defined far below, once the
	// tabs and the window content it swaps between exist. Everything that
	// swallows the keyboard — the serial terminal, the tools terminal, the
	// VNC viewer — needs the toggle at construction time, which is long
	// before that, so the declaration has to come before all three.
	var exitFullScreen, toggleFullScreen func()

	// ── right-top: the console pane ──────────────────────────────────────
	// Zero-interaction contract (operator ask): selecting a running VM
	// attaches its serial console here; moving the selection swaps it;
	// selecting a stopped VM detaches. Click into the pane to type.
	conPlaceholder := func(msg string) fyne.CanvasObject {
		return container.NewCenter(widget.NewLabel(msg))
	}
	consoleHost := container.NewStack(conPlaceholder(
		"select a running VM — its serial console attaches here"))
	detachConsole := func() {
		if st.conCmd != nil && st.conCmd.Process != nil {
			_ = st.conCmd.Process.Kill()
		}
		if st.conPty != nil {
			_ = st.conPty.Close()
		}
		st.conCmd, st.conPty, st.conName = nil, nil, ""
	}
	attachConsole := func(name string) {
		if st.conName == name && st.conCmd != nil {
			return // already on this VM's console
		}
		detachConsole()
		// virsh console through a pty; the terminal widget speaks to the
		// pty directly, so virsh's own errors land visibly in the pane
		v := virsh("console", name)
		cmd := exec.Command(v[0], v[1:]...)
		p, err := pty.Start(cmd)
		if err != nil {
			consoleHost.Objects = []fyne.CanvasObject{
				conPlaceholder("console: " + err.Error())}
			consoleHost.Refresh()
			return
		}
		st.conCmd, st.conPty, st.conName = cmd, p, name
		term := fyneterm.New()
		// A focused terminal swallows the whole keyboard, exactly as the VNC
		// viewer does, so a canvas-level shortcut never fires while the
		// operator is in it — and fullscreen becomes a room with no door.
		// fyneterm delegates CustomShortcuts to its embedded ShortcutHandler
		// before consuming them, so registering there is all it takes.
		term.AddShortcut(fullScreenKey, func(fyne.Shortcut) {
			if toggleFullScreen != nil {
				toggleFullScreen()
			}
		})
		// RECOVER, because this goroutine runs third-party code over a pty we
		// do not control the contents of.
		//
		// A panic on ANY goroutine takes the whole process with it — there is
		// no per-goroutine isolation in Go — so a bug in the terminal widget
		// is indistinguishable, from the operator's side, from vmxplore
		// crashing. It was:
		//
		//   panic: runtime error: index out of range [44] with length 0
		//     fyne-io/terminal@…/term.go:419
		//     fyne-io/terminal.(*Terminal).RunWithConnection
		//     main.runGUI.func10.2()  ->  gui.go:990
		//
		// Observed on onyx 2026-08-20: selecting a freshly created clone
		// attached the serial console, the guest emitted something the
		// widget's parser mishandled, and the entire application died —
		// estate, VNC, tools and all. Three times in ninety seconds.
		//
		// The pane is worth less than the application around it. A serial
		// console that stops rendering is a nuisance; losing the window while
		// mid-task is not, and it takes the log with it. Recovering here turns
		// a fatal into a dead pane plus a line in vmx.log naming the widget.
		//
		// Deliberately NOT restarting the terminal: the input that killed it is
		// still queued on the pty, so a retry loop would panic-recover forever
		// and burn a core. Reselecting the VM builds a fresh one.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					auditLog(fmt.Sprintf(
						"serial console for %s panicked inside the fyne terminal widget (%v) — pane disabled, reselect the VM to retry",
						name, r), 1)
				}
			}()
			_ = term.RunWithConnection(p, p)
		}()
		// no focus steal: arrowing through the list must stay in the list —
		// clicking the pane focuses the terminal when you want to type
		consoleHost.Objects = []fyne.CanvasObject{fitTerminal(term)}
		consoleHost.Refresh()
	}
	// Screen pane: the native RFB viewer (vnc.go) — same auto-follow
	// contract as serial. Loopback makes eager attach cheap.
	vncHost := container.NewStack(conPlaceholder(
		"select a running VM — its graphical console renders here"))
	var vncConn *rfbConn
	// vncTunnel closes the ssh forward carrying a remote console. Nil when
	// the estate is local, or when nothing is attached.
	var vncTunnel func()
	vncName := ""
	detachVNC := func() {
		if vncConn != nil {
			vncConn.Close()
		}
		// Tear the ssh forward down with the console. Without this a remote
		// estate accumulates one listening local port per VM ever opened.
		if vncTunnel != nil {
			vncTunnel()
			vncTunnel = nil
		}
		vncConn, vncName = nil, ""
	}
	attachVNC := func(name string) {
		if vncName == name && vncConn != nil {
			return
		}
		detachVNC()
		port, err := vncPort(lv, name)
		if err != nil {
			vncHost.Objects = []fyne.CanvasObject{conPlaceholder(err.Error())}
			vncHost.Refresh()
			return
		}
		addr, stopTunnel, err := vncEndpoint(port)
		if err != nil {
			vncHost.Objects = []fyne.CanvasObject{conPlaceholder(err.Error())}
			vncHost.Refresh()
			return
		}
		vncTunnel = stopTunnel
		conn, err := dialRFB(addr)
		if err != nil {
			stopTunnel()
			vncTunnel = nil
			vncHost.Objects = []fyne.CanvasObject{
				conPlaceholder("vnc: " + err.Error())}
			vncHost.Refresh()
			return
		}
		vncConn, vncName = conn, name
		// guest clipboard → host clipboard (ServerCutText)
		conn.SetOnCutText(func(s string) {
			fyne.Do(func() { a.Clipboard().SetContent(s) })
		})
		v := newVNCViewer(conn)
		// The viewer forwards the keyboard to the guest, so it has to be told
		// which key is ours. Wrapped rather than assigned directly: the trio
		// is still nil at this point in the build order.
		v.onFullScreen = func() {
			if toggleFullScreen != nil {
				toggleFullScreen()
			}
		}
		// Say when the console dies. The read loop closes done on any terminal
		// error — the guest powered off, the hypervisor went away, the ssh
		// forward dropped, a write failed — and without this the pane keeps
		// showing the last frame it received, which reads as "the console
		// froze" rather than "the connection ended". Err() is nil for a
		// deliberate detach, so only real failures replace the pane, and the
		// identity check means a stale watcher cannot clobber a newer console.
		go func(dead *rfbConn) {
			<-dead.done
			derr := dead.Err()
			if derr == nil {
				return
			}
			fyne.Do(func() {
				if vncConn != dead {
					return
				}
				vncHost.Objects = []fyne.CanvasObject{
					conPlaceholder("console disconnected: " + derr.Error())}
				vncHost.Refresh()
			})
		}(conn)
		// Focus it as soon as it is on screen. Waiting for a click means the
		// first thing an operator meets — a login prompt — cannot be typed
		// into until they happen to click the right place first.
		defer fyne.Do(func() { v.focus() })
		vncHost.Objects = []fyne.CanvasObject{v}
		vncHost.Refresh()
	}

	// ── kldload extras: the tier-3 surface ───────────────────────────────
	// The k-tools are interactive CLIs, and this app embeds a terminal —
	// so Extras run right in the console card ("all 1 app"). The launched
	// tool drops into a shell afterwards so wizards and follow-ups work.
	// On a generic host the same menu pitches the OS instead — the
	// promote-through-the-app strategy, capability-probed as always.
	toolsHost := container.NewStack()
	var toolCmd *exec.Cmd
	var toolPty interface{ Close() error }
	var selectToolsTab func()  // set once the tab bar exists below
	var selectScreenTab func() // ditto — where a new VM's first boot shows
	var hideTools func()       // ditto — the tools tab leaves with its tool
	var showToolTiles func()
	// Where "← back" goes from a running tool. A tool is almost always reached
	// THROUGH its verb page (klab → ztest goldens), so returning to the top
	// grid throws away the level the operator was working in: they came from
	// the ZFS sub-menu, watched a golden build, pressed back and landed on the
	// main kldload menu with no way back to the build they had been running
	// (.131, 2026-08-15). showToolActions points this at itself; the grid
	// resets it. Declared here because showToolActions is defined further down
	// and would not be in scope for runArgv otherwise.
	backFromTool := func() { showToolTiles() }
	stopTool := func() {
		if toolCmd != nil && toolCmd.Process != nil {
			_ = toolCmd.Process.Kill()
		}
		if toolPty != nil {
			_ = toolPty.Close()
		}
		toolCmd, toolPty = nil, nil
	}
	runArgv := func(argv []string) {
		stopTool()
		// exec directly (fixed argv, no shell wrapper): the exit is the
		// pty EOF that returns the pane to the tiles — quitting a tool
		// must land on the launcher, not a stray prompt (operator). The
		// "shell" tile exists for when a prompt IS what you want.
		cmd := exec.Command(argv[0], argv[1:]...)
		if argv[0] == "shell" {
			cmd = exec.Command("bash", "-i")
		}
		label := strings.Join(argv, " ")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		p, err := pty.Start(cmd)
		if err != nil {
			toolsHost.Objects = []fyne.CanvasObject{
				conPlaceholder(label + ": " + err.Error())}
			toolsHost.Refresh()
			return
		}
		toolCmd, toolPty = cmd, p
		term := fyneterm.New()
		// A focused terminal swallows the whole keyboard, exactly as the VNC
		// viewer does, so a canvas-level shortcut never fires while the
		// operator is in it — and fullscreen becomes a room with no door.
		// fyneterm delegates CustomShortcuts to its embedded ShortcutHandler
		// before consuming them, so registering there is all it takes.
		term.AddShortcut(fullScreenKey, func(fyne.Shortcut) {
			if toggleFullScreen != nil {
				toggleFullScreen()
			}
		})
		barLabel := widget.NewLabel(label)
		start := time.Now()
		go func() {
			_ = term.RunWithConnection(p, p)
			fyne.Do(func() {
				if toolCmd != cmd {
					return // the pane already moved on to another tool
				}
				// a tool that exits within seconds just printed usage or an
				// error — KEEP it on screen so the operator can read it
				// (kube-cluster taught this); a real session's exit returns
				// to the launcher
				if time.Since(start) < 3*time.Second {
					stopTool()
					barLabel.SetText(label + " — exited; output above, ← tools to go back")
					return
				}
				stopTool()
				backFromTool()
			})
		}()
		back := widget.NewButtonWithIcon("back", theme.NavigateBackIcon(),
			func() { stopTool(); backFromTool() })
		bar := container.NewBorder(nil, nil,
			back, nil, barLabel)
		toolsHost.Objects = []fyne.CanvasObject{
			container.NewBorder(bar, nil, nil, nil, fitTerminal(term))}
		toolsHost.Refresh()
		if selectToolsTab != nil {
			selectToolsTab()
		}
		w.Canvas().Focus(term)
	}
	// toolIcon picks a themed glyph by what the tool does — the tiles read
	// as a launcher, not a wall of identical buttons.
	toolIcon := func(name string) fyne.Resource {
		switch {
		case name == "kvm-create" || name == "kspawn":
			return theme.ContentAddIcon()
		case name == "kvm-clone":
			return theme.ContentCopyIcon()
		case name == "kvm-delete":
			return theme.DeleteIcon()
		case name == "kvm-snap" || name == "ksnap":
			return theme.StorageIcon()
		case name == "kvm-list":
			return theme.ListIcon()
		case name == "kimage":
			return theme.DownloadIcon()
		case name == "kexport":
			return theme.UploadIcon()
		case strings.HasPrefix(name, "kube-"):
			return theme.GridIcon()
		case strings.HasSuffix(name, "-demo"):
			return theme.MediaPlayIcon()
		}
		return theme.ComputerIcon()
	}
	// toolAccent colours a tile by what the tool DOES, so the launcher is
	// scannable before a single label is read — the same colour language
	// the verb tiles already speak inside a tool (green makes, red
	// destroys, steel reads), lifted one level up to the tools themselves.
	//
	//	green  builds or creates something new
	//	blue   storage: snapshots, images, the ZFS consoles
	//	gold   read-only — looking, never touching
	//	red    destroys, and cannot be undone
	//	purple guided demos (the brand accent: these are the showpieces)
	//
	// Unknown tools fall through to gold rather than a neutral grey: a new
	// k-tool nobody has classified yet is, at worst, safe to look at.
	// The action catalog (the toolAction type and the toolActions table)
	// lives in actions.go: 157 lines of static data that read as logic
	// when inlined here.

	var showToolActions func(tool string)
	fire := func(act toolAction, extra string) {
		argv := make([]string, 0, len(act.argv)+2)
		for _, aa := range act.argv {
			switch aa {
			case "%SEL%":
				r, ok := st.selected()
				if !ok {
					dialog.ShowInformation(act.argv[0],
						"select a VM in the estate list first", w)
					return
				}
				aa = r.D.Name
			case "%DS%":
				r, ok := st.selected()
				if !ok || r.DS == nil {
					dialog.ShowInformation(act.argv[0],
						"select a VM with a local zvol first", w)
					return
				}
				aa = "/dev/zvol/" + r.DS.Name
			}
			argv = append(argv, aa)
		}
		argv = append(argv, strings.Fields(extra)...)
		if act.confirm {
			dialog.ShowConfirm(act.argv[0],
				"run:  "+strings.Join(argv, " "), func(ok bool) {
					if ok {
						runArgv(argv)
					}
				}, w)
			return
		}
		runArgv(argv)
	}
	showToolActions = func(tool string) {
		// A tool launched from here returns HERE, not to the top grid.
		backFromTool = func() { showToolActions(tool) }
		acts := toolActions[tool]
		tiles := make([]fyne.CanvasObject, 0, len(acts))
		for _, act := range acts {
			act := act
			// colour says what a verb does before you read it, in the
			// same language as the tools grid one level up: green
			// builds, red destroys, gold reads. Nothing is left neutral
			// — a grey tile reads as disabled, not as safe.
			col := acGold.at()
			switch {
			case act.confirm:
				col = acRed.at()
			case act.builds:
				col = acGreen.at()
			}
			// A verb this host cannot perform goes grey and says why.
			//
			// Grey is already the disabled colour in this UI by the rule
			// above — nothing else is left neutral — so an unavailable tile
			// reads as unavailable without inventing a new visual language.
			// The DESCRIPTION is replaced with the reason rather than
			// appended to: on a plain-KVM host "the lean blue/green base
			// images, one per distro" is not the useful sentence, "needs
			// klab (kldload)" is. Full text still arrives on hover.
			//
			// WHY GATE AT ALL: vmx ships standalone. Offering klab verbs on
			// a host with no klab meant pressing a green BUILD tile and
			// watching it fail on a zvol path that never existed there.
			if ok, why := Available(act.needs); !ok {
				tiles = append(tiles, newTile(toolIcon(tool), act.label,
					why, tileSubColor(), func() {
						dialog.ShowInformation(act.label+" unavailable", why, w)
					}))
				continue
			}
			tiles = append(tiles, newTile(toolIcon(tool), act.label,
				act.desc, col, func() {
					if act.prompt == "" {
						fire(act, "")
						return
					}
					entry := widget.NewEntry()
					entry.SetPlaceHolder(act.prompt)
					dialog.ShowCustomConfirm(tool+" "+act.label, "Run", "Cancel",
						entry, func(ok bool) {
							if ok {
								fire(act, entry.Text)
							}
						}, w)
				}))
		}
		back := widget.NewButtonWithIcon("tools", theme.NavigateBackIcon(),
			func() { showToolTiles() })
		bar := container.NewBorder(nil, nil, back, nil,
			container.NewCenter(pageHeading(tool, acGold)))
		grid := container.NewGridWrap(tileSize, tiles...)
		toolsHost.Objects = []fyne.CanvasObject{container.NewBorder(
			bar, nil, nil, nil,
			container.NewVScroll(container.NewPadded(grid)))}
		toolsHost.Refresh()
	}
	openTool = func(tool string) {
		if _, ok := toolActions[tool]; ok {
			showToolActions(tool)
			if selectToolsTab != nil {
				selectToolsTab()
			}
			return
		}
		runArgv([]string{tool})
	}

	// Top level. The launcher is the estate tree's "kldload tools" branch,
	// so "back" from a tool or its verb page means: the tools tab has
	// nothing left to show and leaves the console card again.
	showToolTiles = func() {
		backFromTool = func() { showToolTiles() }
		if hideTools != nil {
			hideTools()
		}
	}

	// conName/conState remember what the panes are actually attached TO,
	// as opposed to what is merely selected — the refresh loop compares
	// against these to notice a machine that changed underneath a standing
	// attachment. conLast* are the previous poll's sample, which is what
	// makes a restart detectable at all (see the refresh loop).
	var conName, conState, conLastName string
	var conCPU uint64

	// followConsole keeps both panes in lock-step with the selection.
	followConsole := func(r Row) {
		conName, conState = r.D.Name, r.D.State
		if r.D.State == "running" && !r.Synthetic {
			attachConsole(r.D.Name)
			attachVNC(r.D.Name)
			return
		}
		detachConsole()
		detachVNC()
		msg := r.D.Name + " is " + r.D.State + " — console attaches when it runs"
		consoleHost.Objects = []fyne.CanvasObject{conPlaceholder(msg)}
		consoleHost.Refresh()
		vncHost.Objects = []fyne.CanvasObject{conPlaceholder(msg)}
		vncHost.Refresh()
	}

	// ── right-bottom: dossier + verbs ────────────────────────────────────
	dossier := widget.NewRichText(&widget.TextSegment{
		Text:  "select a VM — everything about it lives here",
		Style: widget.RichTextStyle{TextStyle: fyne.TextStyle{Monospace: true}}})
	dossier.Wrapping = fyne.TextWrapWord // lineage/disk lines run long
	renderDossier := func(r Row) {
		dossier.Segments = st.dossierSegs(r)
		dossier.Refresh()
	}
	status := widget.NewLabel(fmt.Sprintf("vmxplore %s · rules: %s",
		versionFull(), rs.Source))
	// every verb fires without a dialog and reports here
	guiStatus = func(s string) { fyne.Do(func() { status.SetText(s) }) }

	// ── the verb toolbar ─────────────────────────────────────────────────
	// Two tiers, mirroring how operators think: the power verbs are
	// always-visible icon buttons; everything else lives behind labelled
	// dropdown menus (Storage / Configure / Build / Estate) so the pane
	// stays organized as verbs accumulate. Every path funnels through the
	// same plan builders as the TUI, fired the same way.
	var refreshNow func()
	// withSel runs a verb against the selected row.
	//
	// It used to `return` silently when nothing was selected, which made EVERY
	// menu verb -- Snapshot, Rollback, vCPU/memory, Make Golden, Clone -- a
	// dead button that gave no feedback at all. From the operator's seat the
	// tool was simply broken (reported 2026-08-15: "there's no way to select
	// what you want to clone ... it's broken"). Saying so costs one dialog and
	// removes an entire class of "nothing happens".
	withSel := func(f func(Row)) func() {
		return func() {
			r, ok := st.selected()
			if !ok {
				dialog.ShowInformation("Nothing selected",
					"Pick a VM in the estate list first, then choose this action again.\n\n"+
						"(Right-clicking a row selects it and opens the same menu.)", w)
				return
			}
			f(r)
		}
	}
	verb := func(build func(Row) (verbPlan, error)) func() {
		return withSel(func(r Row) {
			p, err := build(r)
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			firePlan(w, p, func() { refreshNow() })
		})
	}

	// name-input dialogs share this shape: label, entry, plan on confirm
	nameDialog := func(title, placeholder string, plan func(Row, string) (verbPlan, error)) func() {
		return withSel(func(r Row) {
			entry := widget.NewEntry()
			entry.SetPlaceHolder(placeholder)
			dialog.ShowCustomConfirm(title, "Next", "Cancel", entry,
				func(ok bool) {
					if !ok {
						return
					}
					p, err := plan(r, strings.TrimSpace(entry.Text))
					if err != nil {
						dialog.ShowError(err, w)
						return
					}
					firePlan(w, p, func() { refreshNow() })
				}, w)
		})
	}

	rollbackDialog := withSel(func(r Row) {
		if r.DS == nil {
			dialog.ShowInformation("rollback", "no local dataset behind this VM", w)
			return
		}
		all := st.snaps[r.DS.Name]
		if len(all) == 0 {
			dialog.ShowInformation("rollback", "no snapshots on "+r.DS.Name, w)
			return
		}
		// newest last, like the TUI snaps pane; selecting computes how many
		// newer snapshots -r will destroy — the plan's warning carries it
		opts := make([]string, len(all))
		for i, s := range all {
			opts[len(all)-1-i] = s // newest first for the dropdown
		}
		sel := widget.NewSelect(opts, nil)
		sel.PlaceHolder = "snapshot…"
		dialog.ShowCustomConfirm("rollback "+r.DS.Name, "Next", "Cancel",
			sel, func(ok bool) {
				if !ok || sel.Selected == "" {
					return
				}
				newer := 0
				for i, s := range all {
					if s == sel.Selected {
						newer = len(all) - i - 1
						break
					}
				}
				p, err := planRollback(r, sel.Selected, newer)
				if err != nil {
					dialog.ShowError(err, w)
					return
				}
				firePlan(w, p, func() { refreshNow() })
			}, w)
	})

	specsDialog := withSel(func(r Row) {
		vcpus := widget.NewEntry()
		vcpus.SetText(fmt.Sprint(r.D.VCPUs))
		mem := widget.NewEntry()
		mem.SetText(fmt.Sprint(r.D.MaxMemKiB / (1024 * 1024)))
		form := container.NewVBox(
			widget.NewLabel("vCPUs:"), vcpus,
			widget.NewLabel("memory (GiB):"), mem)
		dialog.ShowCustomConfirm("vCPU / memory — "+r.D.Name+" (applies next start)",
			"Next", "Cancel", form, func(ok bool) {
				if !ok {
					return
				}
				var c, g int
				if _, err := fmt.Sscanf(strings.TrimSpace(vcpus.Text), "%d", &c); err != nil || c < 1 {
					dialog.ShowError(fmt.Errorf("vcpus must be a positive number"), w)
					return
				}
				if _, err := fmt.Sscanf(strings.TrimSpace(mem.Text), "%d", &g); err != nil || g < 1 {
					dialog.ShowError(fmt.Errorf("memory must be a positive number of GiB"), w)
					return
				}
				p, err := planSpecs(r, c, g)
				if err != nil {
					dialog.ShowError(err, w)
					return
				}
				firePlan(w, p, func() { refreshNow() })
			}, w)
	})

	// Grow the disk, and the partition and filesystem inside it. The size is
	// read first so the field opens on the truth and the shrink guard in
	// planResizeDisk has a real number to compare against — see resize.go for
	// why an unknown current size is a refusal rather than a default.
	resizeDialog := withSel(func(r Row) {
		cur, err := currentDiskBytes(r)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		size := widget.NewEntry()
		size.SetText(fmt.Sprint(cur / gib))
		form := container.NewVBox(
			widget.NewLabel("New size (GiB) — currently "+humanGiB(cur)+":"), size,
			widget.NewLabel("Grows the disk, its partition and its filesystem."),
			widget.NewLabel("One way: a disk cannot be shrunk back."))
		dialog.ShowCustomConfirm("Resize disk — "+r.D.Name, "Grow", "Cancel", form,
			func(ok bool) {
				if !ok {
					return
				}
				var g int
				if _, err := fmt.Sscanf(strings.TrimSpace(size.Text), "%d", &g); err != nil || g < 1 {
					dialog.ShowError(fmt.Errorf("size must be a positive number of GiB"), w)
					return
				}
				p, err := planResizeDisk(r, g, cur)
				if err != nil {
					dialog.ShowError(err, w)
					return
				}
				firePlan(w, p, func() { refreshNow() })
			}, w)
	})

	// menuButton drops a popup menu under the button — the submenu chrome.
	menuButton := func(label string, icon fyne.Resource, items ...*fyne.MenuItem) *widget.Button {
		var b *widget.Button
		b = widget.NewButtonWithIcon(label, icon, func() {
			m := widget.NewPopUpMenu(fyne.NewMenu("", items...), w.Canvas())
			pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(b)
			m.ShowAtPosition(pos.Add(fyne.NewPos(0, b.Size().Height)))
		})
		return b
	}
	soon := func(what, when string) func() {
		return func() {
			dialog.ShowInformation(what,
				what+" lands in "+when+" — designed, not built yet.\n"+
					"The kldload webui does this today.", w)
		}
	}

	// The action row speaks one language, in two families.
	//
	// Lifecycle verbs are traffic lights — green runs it, amber stops it
	// politely, red pulls the plug — so the consequence is legible before
	// the label is read. Everything else is a MENU rather than an act, and
	// those all carry the brand accent so they read as one family and never
	// as something that is about to happen to a machine.
	//
	// The palette is Fyne's Importance rather than hand-painted colour:
	// Success/Warning/Danger/High are the four the theme already resolves
	// per light and dark variant, so the row stays right when GNOME flips
	// and nothing here has to be repainted by applyPalette.
	btnStart := widget.NewButtonWithIcon("Start", theme.MediaPlayIcon(), verb(planStart))
	btnStart.Importance = widget.SuccessImportance
	btnStop := widget.NewButtonWithIcon("Shut down", theme.MediaStopIcon(), verb(planShutdown))
	btnStop.Importance = widget.WarningImportance
	btnKill := widget.NewButtonWithIcon("Force off", theme.ErrorIcon(), verb(planForceOff))
	btnKill.Importance = widget.DangerImportance

	snapAct := nameDialog("zfs snapshot @manual-…",
		"suffix (empty = timestamp)", planSnapshot)

	// New VM: the native cloud-image pipeline (newvm.go). The dialog is a
	// form; the pipeline streams its exact commands into the status bar
	// and the estate refreshes when the domain lands.
	// Declared before assignment so the dialog can re-open ITSELF on
	// invalid input — a closure cannot reference a variable that :=
	// has not finished binding.
	var newVMDialog func()
	newVMDialog = func() {
		name := widget.NewEntry()
		name.SetPlaceHolder("vm name")
		name.Validator = nameValidator()
		// build mode: a cloud image (fast — cloud-init, preset user) or an
		// installer ISO (boot the distro's own installer in the Screen
		// tab, run apt/dnf/pacman the normal way; any ISO — Debian, Fedora,
		// an Arch live ISO, a RHEL DVD)
		mode := widget.NewSelect([]string{"cloud image", "installer ISO"}, nil)
		mode.SetSelected("cloud image")
		distro := widget.NewSelect(append(CloudDistros(), "custom image…"), nil)
		distro.SetSelected("fedora")
		// Cloud images are headless, so every VM was a server whether or not
		// that was wanted. This offers the desktops VERIFIED for the selected
		// distro — the list is generated from the recipe table, so it can
		// never offer a combination the distro's repositories cannot satisfy
		// (see desktop.go). A distro with no verified recipes shows "server"
		// alone rather than a promise nothing can keep.
		desktop := widget.NewSelect(DesktopsFor("fedora"), nil)
		desktop.SetSelected("none")

		// Host audio. Offered only when it can actually work, because the
		// failure mode is not "no sound" — qemu treats an unreachable audio
		// backend as fatal, so a domain wired to one that is not there does
		// not start. hostAudioReachable() asks qemu itself, under the same
		// empty environment libvirt uses.
		//
		// Remote targets never get it: virt-install runs here while the guest
		// runs there, so this session's audio says nothing about that machine
		// — and its sound would come out of that machine's speakers anyway.
		soundOK := target.SSHHost == "" && hostAudioReachable()
		sound := widget.NewCheck("host audio (guest sound plays on this machine)", nil)
		soundNote := widget.NewLabel("")
		soundNote.Wrapping = fyne.TextWrapWord
		soundNote.TextStyle = fyne.TextStyle{Italic: true}
		if soundOK {
			// ON by default, because the answer to "should this VM have
			// sound" is yes whenever it CAN, and soundOK has already proved
			// it can — qemu itself opened the backend, under libvirt's own
			// empty environment.
			//
			// HISTORY: 2026-08-12. Shipped unchecked, and the first VM built
			// after the feature landed had no sound. Nothing failed and
			// nothing was logged: the box was simply never ticked, so the
			// host backend stayed type='none' AND the guest never got its
			// audio stack. That reads as "the sound feature is broken",
			// which is worse than not having it — an unchecked box is
			// indistinguishable from a bug at the only moment anyone looks.
			// Opting OUT of sound is a preference; opting IN should not be a
			// prerequisite for a desktop that makes noise.
			sound.SetChecked(true)
			soundNote.SetText("Adds an ich9 card wired to this session, and " +
				"installs the guest's audio stack on first boot. Untick for " +
				"a silent guest.")
		} else {
			sound.Disable()
			soundNote.SetText(audioHostHint())
		}
		desktopNote := widget.NewLabel("")
		desktopNote.Wrapping = fyne.TextWrapWord
		desktopNote.TextStyle = fyne.TextStyle{Italic: true}
		desktopNote.Hide()
		desktop.OnChanged = func(d string) {
			// A desktop is 1.5–3GB. Saying so is the difference between a
			// slow build and one the operator believes has hung.
			if d == "" || d == "none" {
				desktopNote.Hide()
				// Headless again: sound goes back to being a choice.
				if soundOK {
					sound.Enable()
					soundNote.SetText("Adds an ich9 card wired to this session, and " +
						"installs the guest's audio stack on first boot. Untick for " +
						"a silent guest.")
				}
				return
			}
			desktopNote.SetText("adds 1.5–3GB on first boot — this VM will take " +
				"5–10 minutes instead of about a minute, and the console narrates it")
			desktopNote.Show()

			// A desktop with no sound is not a desktop anyone wants, so this
			// stops being a question the moment one is selected: ticked, and
			// locked. Operator, 2026-08-15 — "no need for a sound check box,
			// just enable it for window manager installs."
			//
			// Still gated on soundOK. qemu treats an unreachable audio backend
			// as FATAL — the domain does not start — so forcing sound on a host
			// that cannot provide it would trade "a desktop with no sound" for
			// "a VM that will not boot", which is a much worse trade.
			if soundOK {
				sound.SetChecked(true)
				sound.Disable()
				soundNote.SetText("Sound is enabled automatically for desktop VMs.")
			}
		}
		imgPath := widget.NewEntry()
		imgPath.SetPlaceHolder("/path/to/image.qcow2 (custom only)")
		imgPath.Hide()
		isoPath := widget.NewEntry()
		isoPath.SetPlaceHolder("/path/to/installer.iso")
		vcpus := widget.NewEntry()
		vcpus.SetText("2")
		vcpus.Validator = numValidator(1, "vcpu")
		ram := widget.NewEntry()
		ram.SetText("2048")
		ram.Validator = numValidator(256, "MB of RAM")
		diskGB := widget.NewEntry()
		diskGB.SetText("20")
		diskGB.Validator = numValidator(2, "GB of disk")
		// Prefilled, not placeholdered: an empty password box and an
		// empty key box build a VM with no way in, and a grey hint is
		// too easy to read as "already handled".
		user := widget.NewEntry()
		user.SetText("admin")
		pass := widget.NewEntry()
		pass.SetText(DefaultGuestPassword)
		key := widget.NewEntry()
		key.SetPlaceHolder("ssh public key (optional)")
		key.SetText(pickPubKey())
		// the custom post-installer: bash run as root on first boot. This
		// is "build your own image" — install packages, drop configs, then
		// Make Golden → Clone. A Load… button reuses a script from disk.
		post := widget.NewMultiLineEntry()
		post.SetPlaceHolder("# post-install bash — runs once, as root, on first boot\n" +
			"# e.g.  dnf install -y nginx && systemctl enable --now nginx")
		post.SetMinRowsVisible(5)
		loadPost := widget.NewButtonWithIcon("Load…", theme.FolderOpenIcon(),
			func() {
				fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
					if err != nil || rc == nil {
						return
					}
					defer rc.Close()
					if b, e := os.ReadFile(rc.URI().Path()); e == nil {
						post.SetText(string(b))
					}
				}, w)
				fd.Show()
			})
		// cloud-only fields hide in installer mode (the guest's installer
		// collects user/packages/layout itself)
		// NVIDIA mode, offered only where it could possibly work.
		//
		// The tool tiers itself by what the host can DO — the same probe
		// rule the kldload toolset follows — so this checkbox exists only
		// when the hypervisor actually has an NVIDIA card in it. On a
		// machine with no GPU it would be several hundred megabytes of
		// driver, ten minutes of build, and nothing to drive.
		//
		// The label says what it does AND what it does not: installing the
		// driver is the guest half. Handing the card to the guest is a
		// host-side passthrough job that this checkbox does not do.
		gpus := HostGPUs()
		nvidia := widget.NewCheck("NVIDIA drivers in the guest", nil)
		nvidiaRow := container.NewVBox(nvidia)
		if NVIDIAHost(gpus) {
			what := "an NVIDIA card"
			for _, g := range gpus {
				if g.IsNVIDIA() && g.Model != "" {
					what = g.Model
					break
				}
			}
			note := widget.NewLabel("host has " + what +
				" — the guest still needs the card passed through to use it")
			if !IOMMUEnabled() {
				note.SetText("host has " + what +
					" — but the IOMMU is off, so no card can be passed to a guest yet")
			}
			note.Wrapping = fyne.TextWrapWord
			note.TextStyle = fyne.TextStyle{Italic: true}
			nvidiaRow.Add(note)
		} else {
			nvidiaRow.Hide()
		}
		cloudOnly := container.NewVBox(
			widget.NewLabel("distro"), distro, imgPath,
			widget.NewLabel("desktop"), desktop, desktopNote,
			widget.NewLabel("sound"), sound, soundNote,
			widget.NewLabel("user"), user,
			widget.NewLabel("password"), pass,
			widget.NewLabel("ssh key"), key,
			nvidiaRow,
			container.NewBorder(nil, nil,
				widget.NewLabel("post-install"), loadPost),
			post)
		isoRow := container.NewVBox(widget.NewLabel("installer ISO"), isoPath)
		isoRow.Hide()
		distro.OnChanged = func(s string) {
			if s == "custom image…" {
				imgPath.Show()
			} else {
				imgPath.Hide()
			}
			// Re-offer only what THIS distro has verified recipes for, and
			// drop a selection the new distro cannot honour rather than
			// carrying it silently into a build that would ignore it.
			opts := DesktopsFor(s)
			desktop.Options = opts
			if !DesktopSupported(s, desktop.Selected) {
				desktop.SetSelected("none")
			}
			desktop.Refresh()
		}
		mode.OnChanged = func(s string) {
			if s == "installer ISO" {
				cloudOnly.Hide()
				isoRow.Show()
			} else {
				cloudOnly.Show()
				isoRow.Hide()
			}
		}
		form := container.NewVBox(
			widget.NewLabel("name"), name,
			widget.NewLabel("source"), mode,
			isoRow,
			cloudOnly,
			container.NewGridWithColumns(3,
				container.NewVBox(widget.NewLabel("vCPUs"), vcpus),
				container.NewVBox(widget.NewLabel("RAM (MB)"), ram),
				container.NewVBox(widget.NewLabel("disk (GB)"), diskGB)),
		)
		d := dialog.NewCustomConfirm("New VM — cloud image or installer ISO",
			"Create", "Cancel", container.NewVScroll(form), func(ok bool) {
				if !ok {
					return
				}
				var c, m, g int
				fmt.Sscanf(strings.TrimSpace(vcpus.Text), "%d", &c)
				fmt.Sscanf(strings.TrimSpace(ram.Text), "%d", &m)
				fmt.Sscanf(strings.TrimSpace(diskGB.Text), "%d", &g)
				spec := NewVMSpec{
					Name:  strings.TrimSpace(name.Text),
					VCPUs: c, RAMMB: m, DiskGB: g,
				}
				if mode.Selected == "installer ISO" {
					spec.ISOPath = strings.TrimSpace(isoPath.Text)
				} else {
					spec.User = strings.TrimSpace(user.Text)
					spec.Password = pass.Text
					spec.SSHKey = strings.TrimSpace(key.Text)
					spec.PostInst = post.Text
					// Composed, not merged: the driver layer runs first
					// and reboots, and cloud-init resumes the operator's
					// own script on the way back up.
					if nvidia.Checked {
						spec.PostInst = nvidiaGuestScript + "\n" + post.Text
					}
					if distro.Selected == "custom image…" {
						spec.ImagePath = strings.TrimSpace(imgPath.Text)
					} else {
						spec.Distro = distro.Selected
						// Only carry a desktop the chosen distro has a
						// verified recipe for. A custom image gets none:
						// we do not know what is inside it, and guessing
						// its package manager is how a build fails ten
						// minutes in.
						if DesktopSupported(spec.Distro, desktop.Selected) {
							spec.Desktop = desktop.Selected
						}
						// Guarded by soundOK as well as the tick: a disabled
						// check cannot be set by a click, but it can be set by
						// a future code path, and wiring a backend that is not
						// reachable produces a VM that will not start.
						spec.Sound = soundOK && sound.Checked
					}
				}
				// The spec knows the rules; ask it before doing anything
				// destructive-adjacent. Invalid input re-opens THIS dialog
				// with the fields intact rather than failing later in the
				// pipeline with the form already gone.
				if !guardConfirm(spec.validate(), newVMDialog, w) {
					return
				}
				done := "created — cloud-init finishes the first boot"
				if spec.install() {
					done = "created — open the Screen tab and run the installer"
				}
				parent := ZFSVMParent(st.visibleRows())
				// Focus the machine being built, and show it. Selection
				// follows the domain BY NAME across refreshes (see the
				// refresh loop), so naming it here means the estate list
				// jumps to the new VM the moment it first appears; the tab
				// switch means the operator is looking at its first boot
				// rather than at the form they just submitted. That boot is
				// the part worth watching and it is over before the build
				// call returns.
				st.selName = spec.Name
				if selectScreenTab != nil {
					selectScreenTab()
				}
				go func() {
					err := BuildNewVM(spec, parent, func(line string) {
						fyne.Do(func() { status.SetText(line) })
					})
					fyne.Do(func() {
						if err != nil {
							dialog.ShowError(err, w)
							return
						}
						refreshNow()
						// The status line, not a modal: the VM is already
						// built and in the list by now, so a popup asking to
						// be dismissed is pure friction between the operator
						// and the machine they just made.
						status.SetText(spec.Name + " " + done)
					})
				}()
			}, w)
		d.Resize(fyne.NewSize(460, 560))
		d.Show()
	}

	// Appliances: the catalog (appliances.go) as a button. Picking an entry
	// fixes the distro and sizing and supplies the post-install script, so
	// the only things left to answer are app-specific — which is the whole
	// point: "give me a blog" instead of "give me a Debian VM, then follow
	// a writeup." The build path is the ordinary New VM pipeline.
	// preselect names the catalog entry to open on; empty picks the first.
	// The left-tree catalog branch passes the entry that was clicked.
	applianceDialog := func(preselect string) {
		catalog := Appliances()
		if len(catalog) == 0 {
			dialog.ShowInformation("Applications", "The catalog is empty.", w)
			return
		}
		if _, ok := ApplianceByName(preselect); !ok {
			preselect = catalog[0].Name
		}
		name := widget.NewEntry()
		name.SetPlaceHolder("vm name")
		pick := widget.NewSelect(ApplianceNames(), nil)
		summary := widget.NewLabel("")
		summary.Wrapping = fyne.TextWrapWord
		notes := widget.NewLabel("")
		notes.Wrapping = fyne.TextWrapWord
		notes.TextStyle = fyne.TextStyle{Italic: true}
		// fields is rebuilt on every pick; entries indexes the live widgets
		// by field key so the submit handler can read them back.
		fields := container.NewVBox()
		entries := map[string]*widget.Entry{}
		current := catalog[0]

		// The VM's own account, and prefilled for the same reason as New
		// VM: this dialog carries TWO credential pairs — the app's admin
		// login above and the machine's login here — and the machine's
		// were the easy ones to leave blank.
		user := widget.NewEntry()
		user.SetText("admin")
		pass := widget.NewEntry()
		pass.SetText(DefaultGuestPassword)
		key := widget.NewEntry()
		key.SetPlaceHolder("ssh public key (optional)")
		key.SetText(pickPubKey())

		rebuild := func(appName string) {
			a, ok := ApplianceByName(appName)
			if !ok {
				return
			}
			current = a
			summary.SetText(fmt.Sprintf("%s — %s\n%s · %s · %d vCPU, %d MB, %d GB",
				a.Name, a.Summary, a.License, a.Distro, a.VCPUs, a.RAMMB, a.DiskGB))
			notes.SetText(a.Notes)
			fields.RemoveAll()
			entries = map[string]*widget.Entry{}
			for _, f := range a.Fields {
				var e *widget.Entry
				if f.Secret {
					e = widget.NewPasswordEntry()
				} else {
					e = widget.NewEntry()
				}
				e.SetText(f.Default)
				e.SetPlaceHolder(f.Placeholder)
				entries[f.Key] = e
				fields.Add(widget.NewLabel(f.Label))
				fields.Add(e)
			}
			fields.Refresh()
		}
		pick.OnChanged = rebuild
		pick.SetSelected(preselect)

		// The two credential blocks are labelled as a pair of opposites
		// on purpose. Filling the app's login and leaving the machine's
		// blank is the mistake this dialog invites, and the result used
		// to be a VM that served its app perfectly and could not be
		// logged into at all.
		appHead := widget.NewLabel("the app's own login — you sign into the website with this")
		appHead.TextStyle = fyne.TextStyle{Bold: true}
		vmHead := widget.NewLabel("the machine's login — console and ssh, not the app")
		vmHead.TextStyle = fyne.TextStyle{Bold: true}
		// A golden is a template, not a member of the estate: it is sealed
		// (machine-id and host keys stripped) and snapshotted @golden, and
		// deliberately NOT enrolled — a mesh key baked into a template is
		// one key shared by every clone. Right-click → Clone then clones
		// out ready copies in seconds instead of running the recipe again.
		goldenChk := widget.NewCheck(
			"seal as a golden when ready — a clone template, not enrolled", nil)
		form := container.NewVBox(
			widget.NewLabel("appliance"), pick, summary,
			widget.NewSeparator(),
			widget.NewLabel("vm name"), name,
			appHead,
			fields,
			widget.NewSeparator(),
			vmHead,
			user, pass, key,
			widget.NewSeparator(),
			goldenChk,
			notes,
		)
		d := dialog.NewCustomConfirm("Application — a configured app in one shot",
			"Build", "Cancel", container.NewVScroll(form), func(ok bool) {
				if !ok {
					return
				}
				vals := map[string]string{}
				for k, e := range entries {
					vals[k] = e.Text
				}
				a := current
				spec, err := a.Spec(name.Text, user.Text, pass.Text, key.Text, vals)
				if err != nil {
					// Field validation lives in the catalog entry, so a bad
					// value is reported here rather than 90 seconds later in
					// the guest's cloud-init log.
					dialog.ShowError(err, w)
					return
				}
				parent := ZFSVMParent(st.visibleRows())
				// Focus the machine being built, and show it. Selection
				// follows the domain BY NAME across refreshes (see the
				// refresh loop), so naming it here means the estate list
				// jumps to the new VM the moment it first appears; the tab
				// switch means the operator is looking at its first boot
				// rather than at the form they just submitted. That boot is
				// the part worth watching and it is over before the build
				// call returns.
				if KldloadTier() == "kldload" {
					if k := hostOpsPubkey(); k != "" {
						spec.RootSSHKeys = append(spec.RootSSHKeys, k)
					}
				}
				st.selName = spec.Name
				if selectScreenTab != nil {
					selectScreenTab()
				}
				asGolden := goldenChk.Checked
				go func() {
					say := func(line string) {
						fyne.Do(func() { status.SetText(line) })
					}
					err := BuildNewVM(spec, parent, say)
					if err == nil {
						AttachUSBDevices(spec.Name, a.USB, say)
					}
					if err == nil && asGolden {
						// Wait for the recipe to answer on its port — that
						// is the proof it finished — then seal. No enrollment
						// (see goldenChk).
						if _, err = WaitAppliance(spec.Name, a.Port, say); err == nil {
							err = SealApplianceGolden(spec.Name, say)
						}
					} else if err == nil {
						// Substrate enrollment: mesh, estate cert, inventory.
						// Narrates in the same status line as the build.
						EnrollAppliance(spec.Name, applianceSlug(a.Name), say)
					}
					fyne.Do(func() {
						if err != nil {
							dialog.ShowError(err, w)
							return
						}
						refreshNow()
						// No modal: the build already narrated itself here,
						// ending with the appliance's real URL, and the
						// catalog entry's notes carry the rest. Where it
						// lands is the one thing worth repeating.
						// The machine login goes in the line too: the
						// app's credentials are in the guest's /root/,
						// but you need to get in to read them.
						status.SetText(fmt.Sprintf(
							"%s — %s ready · %s · machine login %s/%s · "+
								"app credentials in /root/ inside the guest",
							spec.Name, a.Name, a.LandsOn,
							spec.User, spec.Password))
					})
				}()
			}, w)
		d.Resize(fyne.NewSize(480, 620))
		d.Show()
	}
	// close the forward reference the catalog branch in the tree holds
	openAppliance = applianceDialog
	// The self-test window: a live log of the engine building and auditing
	// every tile, exactly what `vmx --selftest` prints. Sequential and slow
	// by nature — the point is proof, and the window says so.
	openSelfTest = func() {
		stw := fyne.CurrentApp().NewWindow("Appliance self-test")
		out := widget.NewLabel("Builds every catalog tile as a real VM, audits the outcome\n" +
			"(recipe verdict, pool, port, mesh, cert, inventory), tears down\n" +
			"passes and keeps failures for diagnosis. Expect ~10-20 min per\n" +
			"ZFS tile on first builds.\n")
		out.Wrapping = fyne.TextWrapWord
		out.TextStyle = fyne.TextStyle{Monospace: true}
		sc := container.NewVScroll(out)
		keep := widget.NewCheck("keep passing VMs too", nil)
		var run *widget.Button
		run = widget.NewButton("Build & audit every tile", func() {
			run.Disable()
			go func() {
				n := SelfTestAppliances("", keep.Checked, func(l string) {
					fyne.Do(func() {
						out.SetText(out.Text + l + "\n")
						sc.ScrollToBottom()
					})
				})
				fyne.Do(func() {
					out.SetText(out.Text + fmt.Sprintf("\ndone — %d tile(s) failed\n", n))
					run.Enable()
				})
			}()
		})
		stw.SetContent(container.NewBorder(
			container.NewHBox(run, keep), nil, nil, nil, sc))
		stw.Resize(fyne.NewSize(720, 560))
		stw.Show()
	}
	// batchLogWindow is the shell the other two catalog-wide verbs share: the
	// self-test window minus its checkbox. job returns the line to append
	// when it is done; auto presses the button on open, for a verb that was
	// already confirmed in a dialog and should not ask twice.
	//
	// Cancel cancels the job's context. A twelve-tile build had no way to
	// stop short of closing the program ("there's no cancel button",
	// operator, 2026-09-04); the job decides what a cancel means — build-all
	// finishes nothing more and removes the tile in flight. The button is
	// live only while a job runs, and a job that ignores ctx (destroy-all,
	// seconds long) simply runs to its end.
	//
	// Progress: a bar and one status line above the log — "tile 5 of 12 ·
	// SDR Station · waiting for the first boot · 3m12s". The counts come
	// from the job through prog; the step is the last log line of the tile
	// in flight; the clock ticks once a second while a job runs. Before
	// this the window was a scrolling log and nothing said how far along a
	// forty-minute run was ("no real indicator or progress indicator",
	// operator, 2026-09-04). A job that never calls prog (destroy-all)
	// shows the clock and the step alone.
	//
	// One window per verb. A second press while one is open focuses it
	// instead of opening another: two presses a second apart opened two
	// windows and started two runs in one process, each on a different
	// tile, heading for a name clash (onyx, 2026-09-04). While a run is
	// going the window refuses to close — the run would carry on unseen.
	batchOpen := map[string]fyne.Window{}
	//
	// autoClose, when non-zero, closes the window that long after a job
	// that did not fail: a destroy or a clone is seconds of log nobody
	// wants to dismiss by hand ("those should auto close in 3 seconds",
	// operator, 2026-09-05). A summary that says FAILED keeps the window,
	// and Build all passes zero because its closing report is the point.
	batchLogWindow := func(title, intro, button string, auto bool, autoClose time.Duration,
		job func(ctx context.Context, log func(string), prog func(done, total int, tile string)) string) {
		if prev, ok := batchOpen[title]; ok {
			prev.RequestFocus()
			return
		}
		bw := fyne.CurrentApp().NewWindow(title)
		batchOpen[title] = bw
		bw.SetOnClosed(func() { delete(batchOpen, title) })
		busy := false
		out := widget.NewLabel(intro)
		out.Wrapping = fyne.TextWrapWord
		out.TextStyle = fyne.TextStyle{Monospace: true}
		sc := container.NewVScroll(out)
		bar := widget.NewProgressBar()
		bar.Hide()
		status := widget.NewLabel("")
		status.TextStyle = fyne.TextStyle{Bold: true}
		var run, cancel *widget.Button
		var stop context.CancelFunc
		run = widget.NewButton(button, func() {
			run.Disable()
			cancel.Enable()
			busy = true
			var ctx context.Context
			ctx, stop = context.WithCancel(context.Background())
			var (
				pmu           sync.Mutex
				pdone, ptotal int
				ptile, pstep  string
				started       = time.Now()
				running       = true
			)
			render := func() {
				pmu.Lock()
				defer pmu.Unlock()
				var parts []string
				if ptotal > 0 {
					n := pdone
					if ptile != "" && n < ptotal {
						n++
					}
					parts = append(parts, fmt.Sprintf("tile %d of %d", n, ptotal))
				}
				if ptile != "" {
					parts = append(parts, ptile)
				}
				if pstep != "" {
					parts = append(parts, pstep)
				}
				if running {
					parts = append(parts, time.Since(started).Round(time.Second).String())
				} else {
					parts = append(parts, "finished in "+time.Since(started).Round(time.Second).String())
				}
				status.SetText(strings.Join(parts, " · "))
				if ptotal > 0 {
					bar.Max = float64(ptotal)
					bar.SetValue(float64(pdone))
					bar.Show()
				}
			}
			tick := time.NewTicker(time.Second)
			go func() {
				for range tick.C {
					fyne.Do(render)
				}
			}()
			go func() {
				sum := job(ctx, func(l string) {
					pmu.Lock()
					// the step is the tile's own line, without its "[vm] " tag
					if i := strings.Index(l, "] "); strings.HasPrefix(l, "[") && i > 0 {
						pstep = strings.TrimSpace(l[i+2:])
					}
					pmu.Unlock()
					fyne.Do(func() {
						out.SetText(out.Text + l + "\n")
						sc.ScrollToBottom()
						render()
					})
				}, func(done, total int, tile string) {
					pmu.Lock()
					pdone, ptotal, ptile = done, total, tile
					if tile != "" {
						pstep = "starting"
					}
					pmu.Unlock()
					fyne.Do(render)
				})
				tick.Stop()
				fyne.Do(func() {
					pmu.Lock()
					running = false
					ptile, pstep = "", ""
					pmu.Unlock()
					render()
					out.SetText(out.Text + "\n" + sum + "\n")
					run.Enable()
					cancel.Disable()
					busy = false
					stop()
					if autoClose > 0 && !strings.Contains(sum, "FAILED") {
						status.SetText(status.Text + fmt.Sprintf(" · closing in %ds", int(autoClose.Seconds())))
						time.AfterFunc(autoClose, func() { fyne.Do(bw.Close) })
					}
				})
			}()
		})
		bw.SetCloseIntercept(func() {
			if busy {
				status.SetText("still running — Cancel first, or leave it open")
				return
			}
			bw.Close()
		})
		cancel = widget.NewButton("Cancel", func() {
			cancel.Disable()
			if stop != nil {
				stop()
			}
		})
		cancel.Disable()
		bw.SetContent(container.NewBorder(
			container.NewVBox(container.NewHBox(run, cancel), status, bar), nil, nil, nil, sc))
		bw.Resize(fyne.NewSize(720, 560))
		bw.Show()
		bw.RequestFocus() // in front of a fullscreen main window, not behind it
		if auto {
			run.OnTapped()
		}
	}
	// auto: the tile already says "Build all", and a window whose only
	// content was that same button again read as nothing happening — the
	// operator clicked the tile, saw no build start, and asked where the
	// feedback was (onyx, 2026-09-04). Build-all skips what exists and is
	// safe to fire, so the tile click IS the press; the window is the log.
	openBuildAll = func() {
		// The tile press itself is audited: on 2026-09-04 the operator
		// clicked Build all twice and saw nothing, and the log could not say
		// whether the click ever arrived. Now it can.
		auditLog("gui: Build all tile pressed", 0)
		batchLogWindow("Build every appliance",
			"Building every catalog tile as a kept VM named app-<tile>. Tiles\n"+
				"whose VM already exists are skipped, so this is also \"build\n"+
				"whatever is missing\". Tiles build one at a time, each with most\n"+
				"of this host's cores and spare RAM while it installs, then trimmed\n"+
				"to its catalog size once it answers, verified, then shut off.\n"+
				"Expect a few minutes per tile; the report at the end says where\n"+
				"each one is and how to log in, and the estate starts it.\n"+
				"Cancel starts nothing more and removes the tile in flight, so\n"+
				"the next run rebuilds it.\n",
			"Build all", true, 0, func(ctx context.Context, log func(string), prog func(int, int, string)) string {
				// The tile the operator clicked says what the run is doing.
				tileSays := func(text string) {
					fyne.Do(func() {
						buildAllStatus = text
						tree.Refresh()
					})
				}
				tileSays("running — starting")
				built, failed, access, urls := BuildAllAppliances(ctx, "", log,
					func(done, total int, tile string) {
						if tile != "" {
							tileSays(fmt.Sprintf("running — tile %d of %d · %s", done+1, total, tile))
						}
						prog(done, total, tile)
					})
				tileSays(fmt.Sprintf("last run %s — %d built, %d failed", time.Now().Format("15:04"), built, failed))
				// One button, then the login pages: every tile that came up
				// opens in its own browser tab, the RDP one in Remmina via
				// its rdp:// handler. Spaced so the browser keeps them in
				// one window instead of racing twelve launches.
				for i, u := range urls {
					if parsed, err := url.Parse(u); err == nil {
						if i > 0 {
							time.Sleep(700 * time.Millisecond)
						}
						if err := fyne.CurrentApp().OpenURL(parsed); err != nil {
							log("could not open " + u + ": " + err.Error())
						}
					}
				}
				// The very last thing in the window: where each tile is
				// and how to get in, in one place, after the noise.
				sum := fmt.Sprintf("done — %d built, %d failed", built, failed)
				if len(access) > 0 {
					sum += "\n\n── where they are, and how to log in ──\n" +
						strings.Join(access, "\n")
				}
				return sum
			})
	}
	// Destroy-all confirms with the exact list it will act on — the same
	// call that then does the acting, so the dialog cannot promise one set
	// of VMs and remove another.
	openDestroyAll = func() {
		vms := ExistingApplianceVMs()
		if len(vms) == 0 {
			dialog.ShowInformation("Destroy all",
				"nothing to remove — no app-* or st-* VM exists", w)
			return
		}
		dialog.ShowConfirm("Destroy every appliance VM",
			"Removes, with their disks, data disks, mesh and inventory rows:\n\n  "+
				strings.Join(vms, "\n  ")+"\n\nNo other VM is touched. There is no undo.",
			func(ok bool) {
				if !ok {
					return
				}
				batchLogWindow("Destroy every appliance", "", "Destroy all", true, 3*time.Second,
					func(_ context.Context, log func(string), _ func(int, int, string)) string {
						return fmt.Sprintf("done — %d removed", DestroyAllAppliances(log))
					})
			}, w)
	}

	// Clone: a golden, a count, a size → kfire clone --wait in the batch
	// window, one line per instance as it answers. Cancel kills the clone
	// mid-loop; whatever was cloned stays listed and can be deleted.
	// Make a golden: the shut-off appliance VMs with a zvol behind them are
	// the candidates; the plan is the same one the row's context menu runs.
	openFCMakeGolden = func() {
		auditLog("gui: Make a golden (Firecracker) pressed", 0)
		var cands []Row
		for _, r := range st.rows {
			if r.FC == nil && !r.Synthetic && r.DS != nil && r.D.State == "shut off" {
				cands = append(cands, r)
			}
		}
		if len(cands) == 0 {
			dialog.ShowInformation("Make a Firecracker golden",
				"No candidate: a golden is taken from a SHUT-OFF VM with a zvol\n"+
					"behind it (the appliance layout). Build a tile, shut it down,\n"+
					"then come back here.", w)
			return
		}
		names := make([]string, len(cands))
		for i, r := range cands {
			names[i] = r.D.Name
		}
		sel := widget.NewSelect(names, nil)
		sel.SetSelected(names[0])
		dialog.ShowCustomConfirm("Make a Firecracker golden", "Make golden", "Cancel",
			container.NewVBox(widget.NewLabel("Snapshot this VM's zvols @kfire and pull its kernel out.\nThe VM itself is untouched."), sel),
			func(ok bool) {
				if !ok {
					return
				}
				for _, r := range cands {
					if r.D.Name == sel.Selected {
						p, err := planFCGolden(r)
						if err != nil {
							dialog.ShowError(err, w)
							return
						}
						firePlan(w, p, func() {
							fcInvalidate()
							refreshNow()
						})
					}
				}
			}, w)
	}
	// Destroy all: the list it will act on is the list it acts on.
	openFCDestroyAll = func() {
		names := make([]string, 0, len(fcRowsNow))
		for _, r := range fcRowsNow {
			names = append(names, r.D.Name)
		}
		if len(names) == 0 {
			dialog.ShowInformation("Destroy all microVMs", "nothing to remove — no Firecracker instance exists", w)
			return
		}
		dialog.ShowConfirm("Destroy every Firecracker microVM",
			"Removes, with their zvol clones, taps, seeds, units and estate rows:\n\n  "+
				strings.Join(names, "\n  ")+"\n\nGoldens are kept. There is no undo.",
			func(ok bool) {
				if !ok {
					return
				}
				batchLogWindow("Destroy every Firecracker microVM", "", "Destroy all", true, 3*time.Second,
					func(ctx context.Context, log func(string), _ func(int, int, string)) string {
						err := streamCmd(ctx, log, kfireArgv("destroy", "--all")...)
						fcInvalidate()
						if err != nil {
							return "destroy FAILED — " + err.Error()
						}
						return "done"
					})
			}, w)
	}
	openFCClone = func() { openFCCloneFor("") }
	openFCCloneFor = func(preselect string) {
		auditLog("gui: Clone microVMs tile pressed", 0)
		if !kfireAvailable() {
			dialog.ShowInformation("Clone microVMs",
				"kfire is not on this host. It ships with kldload's KVM host;\n"+
					"Firecracker itself is installed at firstboot.", w)
			return
		}
		goldens, err := fcGoldens()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		if len(goldens) == 0 {
			dialog.ShowInformation("Clone microVMs",
				"No Firecracker golden yet.\n\nShut an appliance VM down, then right-click it\n"+
					"→ Firecracker golden. Cloning clones that.", w)
			return
		}
		names := make([]string, len(goldens))
		for i, g := range goldens {
			names[i] = g.Name
		}
		sel := widget.NewSelect(names, nil)
		sel.SetSelected(names[0])
		for _, n := range names {
			if n == preselect {
				sel.SetSelected(n)
			}
		}
		count := widget.NewEntry()
		count.SetText("1")
		cpu := widget.NewEntry()
		cpu.SetPlaceHolder("the golden's")
		ram := widget.NewEntry()
		ram.SetPlaceHolder("the golden's, in MB")
		form := widget.NewForm(
			widget.NewFormItem("Golden", sel),
			widget.NewFormItem("How many", count),
			widget.NewFormItem("vCPU", cpu),
			widget.NewFormItem("RAM", ram))
		dialog.ShowCustomConfirm("Clone Firecracker microVMs", "Clone", "Cancel", form, func(ok bool) {
			if !ok {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(count.Text))
			if err != nil || n < 1 {
				dialog.ShowError(fmt.Errorf("how many: a positive number"), w)
				return
			}
			args := []string{"clone", sel.Selected, "-n", fmt.Sprint(n), "--wait"}
			if c := strings.TrimSpace(cpu.Text); c != "" {
				args = append(args, "--cpu", c)
			}
			if m := strings.TrimSpace(ram.Text); m != "" {
				args = append(args, "--ram", m)
			}
			batchLogWindow("Clone Firecracker microVMs",
				fmt.Sprintf("Cloning %d microVM(s) from %s. Each is a ZFS clone of the\n"+
					"golden's zvols and a Firecracker process; the line for each one\n"+
					"prints when it answers on its port. They appear in the estate\n"+
					"under \"firecracker\" and are removed with Delete.\n", n, sel.Selected),
				"Clone", true, 3*time.Second, func(ctx context.Context, log func(string), _ func(int, int, string)) string {
					err := streamCmd(ctx, log, kfireArgv(args...)...)
					fcInvalidate()
					if err != nil {
						return "clone FAILED — " + err.Error()
					}
					return "done — the instances are under \"firecracker\" in the estate"
				})
		}, w)
	}

	// EZ Fleet: one dialog → build a golden + N clones. The whole value
	// proposition in a gesture ("give me 5 Fedora boxes").
	// Declared before assignment so the dialog can re-open ITSELF on
	// invalid input — a closure cannot reference a variable that :=
	// has not finished binding.
	var fleetDialog func()
	// vmNameTaken reports whether a domain OR its zvol already exists. Both
	// matter: virsh undefine leaves the dataset behind, so a name can be free
	// to libvirt and still collide in ZFS.
	vmNameTaken := func(n string) bool {
		if n == "" {
			return false
		}
		// virsh() and not bare "virsh": the latter defaults to
		// qemu:///session locally and, when driving a remote hypervisor,
		// asks THIS box whether a name on the OTHER one is free — which it
		// always is, so the check passed and the create collided.
		di := virsh("dominfo", n)
		if exec.Command(di[0], di[1:]...).Run() == nil {
			return true
		}
		if parent := ZFSVMParent(st.visibleRows()); parent != "" {
			chk := zfsArgv("list", parent+"/"+n)
			if exec.Command(chk[0], chk[1:]...).Run() == nil {
				return true
			}
		}
		return false
	}

	fleetDialog = func() {
		name := widget.NewEntry()
		// A FREE default, not the literal string "fleet".
		//
		// It used to be hardcoded to "fleet", so the first fleet you ever
		// built took that name and every attempt afterwards opened a dialog
		// pre-filled with a name that could not work: click Build, get
		// "dataset fleet exists", with no hint that the fix was to type a
		// different name in a field that the dialog's scroll had often pushed
		// out of sight. Reported 2026-08-15 as "the fleet doesn't work".
		// REQUIRED, and deliberately empty. It used to arrive pre-filled with
		// the literal "fleet", so the first fleet took that name and every
		// attempt afterwards opened a dialog whose default could not work:
		// click Build, get "dataset fleet exists", with no hint that the fix
		// was to type over a field the dialog's scroll had pushed out of
		// sight. An empty required field asks the question instead of
		// answering it wrongly (operator, 2026-08-15).
		name.SetPlaceHolder("required — the golden takes this name, clones become name-1 … name-N")
		name.Validator = nameValidator()
		distro := widget.NewSelect(CloudDistros(), nil)
		distro.SetSelected("fedora")
		// The golden carries the desktop and every clone inherits it, so
		// this is the cheapest place in the product to get a desktop fleet:
		// the 1.5-3GB install happens ONCE, on the golden, and the clones
		// are ZFS clones of a disk that already has it.
		fleetDesktop := widget.NewSelect(DesktopsFor("fedora"), nil)
		fleetDesktop.SetSelected("none")
		fleetDesktopNote := widget.NewLabel("")
		fleetDesktopNote.Wrapping = fyne.TextWrapWord
		fleetDesktopNote.TextStyle = fyne.TextStyle{Italic: true}
		fleetDesktopNote.Hide()
		fleetDesktop.OnChanged = func(d string) {
			if d == "" || d == "none" {
				fleetDesktopNote.Hide()
				return
			}
			fleetDesktopNote.SetText("installed once on the golden, then cloned — " +
				"adds 5–10 minutes to the golden, nothing to each clone")
			fleetDesktopNote.Show()
		}
		distro.OnChanged = func(sel string) {
			fleetDesktop.Options = DesktopsFor(sel)
			if !DesktopSupported(sel, fleetDesktop.Selected) {
				fleetDesktop.SetSelected("none")
			}
			fleetDesktop.Refresh()
		}
		count := widget.NewEntry()
		count.SetText("5")
		count.Validator = numValidator(1, "clone")
		ram := widget.NewEntry()
		ram.SetText("2048")
		ram.Validator = numValidator(256, "MB of RAM")
		diskGB := widget.NewEntry()
		diskGB.SetText("20")
		diskGB.Validator = numValidator(2, "GB of disk")
		post := widget.NewMultiLineEntry()
		post.SetPlaceHolder("# optional post-install bash — baked into every clone")
		post.SetMinRowsVisible(4)
		form := container.NewVBox(
			widget.NewLabel("base name (clones: name-1…name-N)"), name,
			widget.NewLabel("distro"), distro,
			widget.NewLabel("desktop"), fleetDesktop, fleetDesktopNote,
			container.NewGridWithColumns(3,
				container.NewVBox(widget.NewLabel("clones"), count),
				container.NewVBox(widget.NewLabel("RAM (MB)"), ram),
				container.NewVBox(widget.NewLabel("disk (GB)"), diskGB)),
			widget.NewLabel("post-install (optional)"), post,
		)
		d := dialog.NewCustomConfirm("EZ Fleet — golden + clones in one shot",
			"Build", "Cancel", container.NewVScroll(form), func(ok bool) {
				if !ok {
					return
				}
				var n, m, g int
				fmt.Sscanf(strings.TrimSpace(count.Text), "%d", &n)
				fmt.Sscanf(strings.TrimSpace(ram.Text), "%d", &m)
				fmt.Sscanf(strings.TrimSpace(diskGB.Text), "%d", &g)
				spec := NewVMSpec{
					Name: strings.TrimSpace(name.Text), Distro: distro.Selected,
					VCPUs: 2, RAMMB: m, DiskGB: g,
					User: "admin", Password: "kldload", PostInst: post.Text,
				}
				if DesktopSupported(spec.Distro, fleetDesktop.Selected) {
					spec.Desktop = fleetDesktop.Selected
				}
				// Same guard as New VM. A fleet builds a golden AND n clones,
				// so a bad field here wastes considerably more of the
				// operator's evening than a single VM would.
				if err := spec.validate(); err != nil {
					if !guardConfirm(err, fleetDialog, w) {
						return
					}
				}
				if n < 1 {
					if !guardConfirm(errors.New("a fleet needs at least 1 clone"),
						fleetDialog, w) {
						return
					}
				}
				if spec.Name == "" {
					dialog.ShowError(errors.New("give the fleet a name — "+
						"the golden takes it and the clones become name-1 … name-N"), w)
					return
				}
				// Check EVERY name this run will create, before creating any of
				// them. A fleet is a golden plus n clones and takes minutes;
				// discovering the collision on clone 4 of 5 wastes all of it and
				// leaves a half-built fleet behind. Names are checked against
				// both libvirt and ZFS because `virsh undefine` leaves the zvol,
				// so a name can be free to one and taken by the other.
				var taken []string
				if vmNameTaken(spec.Name) {
					taken = append(taken, spec.Name)
				}
				for i := 1; i <= n; i++ {
					cn := fmt.Sprintf("%s-%d", spec.Name, i)
					if vmNameTaken(cn) {
						taken = append(taken, cn)
					}
				}
				if len(taken) > 0 {
					dialog.ShowError(fmt.Errorf(
						"these names already exist: %s\n\nPick a different base name, "+
							"or remove them first (Storage → destroy, or `virsh undefine` "+
							"plus `zfs destroy`).", strings.Join(taken, ", ")), w)
					return
				}
				parent := ZFSVMParent(st.visibleRows())
				// the golden appears first; the clones land under it
				st.selName = spec.Name
				if selectScreenTab != nil {
					selectScreenTab()
				}
				go func() {
					err := BuildFleet(spec, n, parent, func(line string) {
						fyne.Do(func() { status.SetText(line) })
					})
					fyne.Do(func() {
						if err != nil {
							dialog.ShowError(err, w)
							return
						}
						refreshNow()
						status.SetText(fmt.Sprintf(
							"%s golden + %d clones ready", spec.Name, n))
					})
				}()
			}, w)
		d.Resize(fyne.NewSize(460, 520))
		d.Show()
	}

	// Make Golden: shutdown → seal → @golden snapshot (golden.go). Then
	// clones boot as fresh machines.
	goldenAct := func() {
		r, ok := st.selected()
		if !ok {
			return
		}
		dialog.ShowConfirm("Make Golden",
			"Seal "+r.D.Name+" and snapshot it @golden?\n"+
				"It will be shut down, sysprepped, and become a clone template.",
			func(ok bool) {
				if !ok {
					return
				}
				go func() {
					err := MakeGolden(r, func(line string) {
						fyne.Do(func() { status.SetText(line) })
					})
					fyne.Do(func() {
						if err != nil {
							dialog.ShowError(err, w)
							return
						}
						refreshNow()
					})
				}()
			}, w)
	}
	// cloneAny picks the golden clone when a @golden anchor exists, else a
	// fresh-snapshot clone — one menu entry, right behaviour each time.
	// hasGolden reports whether a row's dataset carries a @golden anchor.
	//
	// zfsArgv, not a bare zfs: this decides WHICH clone plan runs, and asking
	// the LOCAL box whether a REMOTE dataset has a @golden snapshot answers
	// about the wrong machine — it would silently pick the full-copy plan for
	// a golden that exists.
	hasGolden := func(r Row) bool {
		if r.DS == nil {
			return false
		}
		chk := zfsArgv("list", r.DS.Name+"@golden")
		return exec.Command(chk[0], chk[1:]...).Run() == nil
	}

	// cloneAny — pick a source, say how many, get that many machines.
	//
	// It used to require a row to already be selected and then silently do
	// NOTHING when one was not, and even when it worked it made exactly one
	// clone with no way to ask for more. The whole point of a golden is
	// cloning out copies (a clone here is a ZFS clone: 17-460 MB and about
	// 0.2s), so "which golden, and how many" is the entire question and
	// neither half could be answered. Reported 2026-08-15: "you should be
	// able to select the golden images and spit out as many as you like".
	//
	// Goldens are listed first and marked, because they are what anyone
	// opening this dialog is looking for.
	cloneAny := func() {
		// ── What you can clone from ──────────────────────────────────
		// One entry per candidate, carrying its own right-hand detail so
		// the list renderer stays a dumb painter.
		type src struct {
			label  string // canonical key: the VM name, or "new golden from <distro>"
			detail string // what it is, what state, how big
			row    Row
			gold   bool
		}
		var goldens, others []src
		for _, r := range st.rows {
			if r.Synthetic {
				continue
			}
			e := src{label: r.D.Name, row: r, gold: hasGolden(r)}
			bits := make([]string, 0, 3)
			if e.gold {
				bits = append(bits, "golden")
			}
			bits = append(bits, r.D.State)
			if r.DS != nil {
				bits = append(bits, humanBytes(r.DS.Used))
			}
			e.detail = strings.Join(bits, " · ")
			if e.gold {
				goldens = append(goldens, e)
			} else {
				others = append(others, e)
			}
		}
		// ...and the option to make a golden that does not exist yet.
		//
		// Clone and "EZ Fleet" were two menu entries asking the same question —
		// what do I clone from, and how many — and differing only in whether
		// the source already existed. That split made the tool feel disjointed
		// and left the honest answer ("you have no goldens yet") hidden behind
		// the wrong menu (operator, 2026-08-15: "we only need 1 menu").
		newPrefix := "new golden from "
		builds := make([]src, 0, len(CloudDistros()))
		for _, d := range CloudDistros() {
			builds = append(builds, src{
				label:  newPrefix + d,
				detail: "builds one first · ~5-10 min",
			})
		}

		byLabel := make(map[string]Row, len(goldens)+len(others))
		for _, e := range goldens {
			byLabel[e.label] = e.row
		}
		for _, e := range others {
			byLabel[e.label] = e.row
		}
		isNew := func(sel string) (string, bool) {
			if strings.HasPrefix(sel, newPrefix) {
				return strings.TrimPrefix(sel, newPrefix), true
			}
			return "", false
		}

		name := widget.NewEntry()
		name.SetPlaceHolder("blank = generate a name for each machine")
		count := widget.NewEntry()
		count.SetText("1")
		count.Validator = numValidator(1, "clone")

		// Fields that only make sense when building a NEW golden. Hidden for a
		// plain clone, because a clone inherits all of this from its source and
		// showing it would imply otherwise.
		newDesktop := widget.NewSelect(DesktopsFor("fedora"), nil)
		newDesktop.SetSelected("none")
		newRAM := widget.NewEntry()
		newRAM.SetText("2048")
		newRAM.Validator = numValidator(256, "MB of RAM")
		newDisk := widget.NewEntry()
		newDisk.SetText("20")
		newDisk.Validator = numValidator(2, "GB of disk")
		newNote := widget.NewLabel("")
		newNote.Wrapping = fyne.TextWrapWord
		newNote.TextStyle = fyne.TextStyle{Italic: true}
		newBox := container.NewVBox(
			widget.NewLabel("desktop (installed once on the golden, inherited by every clone)"),
			newDesktop, newNote,
			container.NewGridWithColumns(2,
				container.NewVBox(widget.NewLabel("RAM (MB)"), newRAM),
				container.NewVBox(widget.NewLabel("disk (GB)"), newDisk)),
		)
		newBox.Hide()

		// ── The picker is a LIST, not a dropdown ─────────────────────
		//
		// It was a widget.Select inside a VBox inside a VScroll, which shows
		// the estate one closed line at a time and gives the dialog no reason
		// to be any bigger than that line: "an undersized box with no real
		// list", "showing like 1 line at a time you can't see anything"
		// (operator, 2026-08-15). The question here is "which image", and
		// nobody can answer it without seeing what they have.
		//
		// Goldens only, by default. Every other VM is one checkbox away rather
		// than gone, because this dialog is the only clone path in the GUI and
		// silently dropping ordinary VMs would remove the ability, not just
		// tidy the list.
		showAll := widget.NewCheck("also list ordinary VMs (full copy, not an instant clone)", nil)
		// A golden is shut off, so its clones arrive shut off, and four
		// powered-off definitions are indistinguishable from the button having
		// done nothing at all ("I said I want 4 debian desktops and I get
		// nothing", operator 2026-08-15). Cloning a machine and leaving it
		// dark is not the feature; the default is to bring them up.
		//
		// It stays a choice because booting a large batch at once is a real
		// load — twenty desktops racing for the same disk is a host nobody can
		// use — and someone cloning a shelf of images for later wants them
		// cold.
		startAfter := widget.NewCheck("power them on once cloned", nil)
		startAfter.SetChecked(true)
		var view []src
		selectedLabel := ""

		list := widget.NewList(
			func() int { return len(view) },
			func() fyne.CanvasObject {
				n := widget.NewLabel("")
				d := widget.NewLabel("")
				d.TextStyle = fyne.TextStyle{Italic: true}
				return container.NewBorder(nil, nil, nil, d, n)
			},
			func(id widget.ListItemID, o fyne.CanvasObject) {
				if id < 0 || id >= len(view) {
					return
				}
				box := o.(*fyne.Container)
				// Border puts the center object first, then the edges in the
				// order they were passed — center, then right.
				box.Objects[0].(*widget.Label).SetText(view[id].label)
				box.Objects[1].(*widget.Label).SetText(view[id].detail)
			},
		)

		// onPicked keeps every reference below working on a label, exactly as
		// the Select did — the list only changes how one gets chosen.
		onPicked := func(sel string) {
			selectedLabel = sel
			d, building := isNew(sel)
			if !building {
				newBox.Hide()
				return
			}
			// Offer only the desktops that distro actually has a verified
			// recipe for, so the list cannot promise what the repo will
			// refuse ten minutes into a build.
			//
			// WARN: keep the operator's choice when the new distro also has a
			// recipe for it. This used to reset to "none" on EVERY list
			// selection, so picking GNOME and then touching the list again
			// silently put it back — and because "none" is a valid answer, the
			// build went ahead and produced headless servers with nothing
			// anywhere saying why. Three Fedora "GNOME desktops" came up as
			// three Fedora servers that way (fiend, 2026-08-17).
			want := newDesktop.Selected
			newDesktop.Options = DesktopsFor(d)
			if !DesktopSupported(d, want) {
				want = "none"
			}
			newDesktop.SetSelected(want)
			newNote.SetText("builds a golden first (~5-10 min, plus 1.5-3GB if a desktop is " +
				"chosen), then clones the clones from it")
			newBox.Show()
		}
		list.OnSelected = func(id widget.ListItemID) {
			if id >= 0 && id < len(view) {
				onPicked(view[id].label)
			}
		}

		// rebuild recomputes the visible rows and keeps the current pick
		// selected when it survives the toggle.
		rebuild := func() {
			view = view[:0]
			view = append(view, goldens...)
			if showAll.Checked {
				view = append(view, others...)
			}
			view = append(view, builds...)
			list.Refresh()
			at := -1
			for i, e := range view {
				if e.label == selectedLabel {
					at = i
					break
				}
			}
			if at < 0 && len(view) > 0 {
				at = 0
			}
			if at >= 0 {
				list.Select(at)
				onPicked(view[at].label)
			}
		}
		showAll.OnChanged = func(bool) { rebuild() }

		// Default to whatever is selected in the estate, so the dialog keeps
		// working the way it always did for anyone who selected a row first.
		// With no goldens at all the first entry is "new golden from …", which
		// is exactly the right default on a fresh host.
		if r, ok := st.selected(); ok {
			selectedLabel = r.D.Name
			// Selecting a plain VM in the estate is a deliberate ask for it,
			// so reveal the section that contains it rather than silently
			// landing on something else.
			for _, e := range others {
				if e.label == selectedLabel {
					showAll.Checked = true
					break
				}
			}
		}
		rebuild()

		heading := widget.NewLabel("Clone from")
		heading.TextStyle = fyne.TextStyle{Bold: true}
		bottom := container.NewVBox(
			showAll, startAfter,
			widget.NewSeparator(),
			container.NewGridWithColumns(2,
				container.NewVBox(widget.NewLabel("name"), name),
				container.NewVBox(widget.NewLabel("how many"), count)),
			newBox,
		)
		// Border, not VBox-in-a-VScroll: the list takes every pixel the fixed
		// size leaves over, which is the whole point of giving it one.
		content := container.NewBorder(heading, bottom, nil, nil, list)

		d := dialog.NewCustomConfirm("Clone out machines — clone a golden, or build one",
			"Go", "Cancel", content, func(ok bool) {
				if !ok {
					return
				}
				var n int
				fmt.Sscanf(strings.TrimSpace(count.Text), "%d", &n)
				if n < 1 {
					n = 1
				}
				base := strings.TrimSpace(name.Text)

				// Building a new golden is the fleet path: one golden plus N
				// clones, all from a cloud image.
				if distro, building := isNew(selectedLabel); building {
					parent := ZFSVMParent(st.visibleRows())
					var m, g int
					fmt.Sscanf(strings.TrimSpace(newRAM.Text), "%d", &m)
					fmt.Sscanf(strings.TrimSpace(newDisk.Text), "%d", &g)
					if base == "" {
						// A golden outlives the clones taken off it, so it is
						// named for what it is rather than "clone-…".
						gen, err := freshName("golden-", map[string]bool{}, parent)
						if err != nil {
							dialog.ShowError(err, w)
							return
						}
						base = gen
					}
					var taken []string
					if nameInUse(base, parent) {
						taken = append(taken, base)
					}
					for i := 1; i <= n; i++ {
						if cn := fmt.Sprintf("%s-%d", base, i); nameInUse(cn, parent) {
							taken = append(taken, cn)
						}
					}
					if len(taken) > 0 {
						dialog.ShowError(fmt.Errorf("these names already exist: %s\n\n"+
							"Pick a different base name, or remove them first.",
							strings.Join(taken, ", ")), w)
						return
					}
					spec := NewVMSpec{
						Name: base, Distro: distro, VCPUs: 2, RAMMB: m, DiskGB: g,
						User: "admin", Password: "kldload",
					}
					// Refuse rather than silently downgrade. Skipping the
					// assignment leaves Desktop empty, which builds a server
					// — the operator asked for a desktop and would not find
					// out until they looked at a blank screen.
					if !DesktopSupported(distro, newDesktop.Selected) {
						dialog.ShowError(fmt.Errorf(
							"no verified %s recipe for %s.\n\n"+
								"Pick another desktop, or install it yourself: put the "+
								"dnf/apt commands in the post-install field. That runs on "+
								"the golden and every clone inherits it.",
							newDesktop.Selected, distro), w)
						return
					}
					spec.Desktop = newDesktop.Selected
					if err := spec.validate(); err != nil {
						dialog.ShowError(err, w)
						return
					}
					st.selName = spec.Name
					if selectScreenTab != nil {
						selectScreenTab()
					}
					go func() {
						err := BuildFleet(spec, n, parent, func(line string) {
							fyne.Do(func() { status.SetText(line) })
						})
						fyne.Do(func() {
							if err != nil {
								dialog.ShowError(err, w)
							}
							refreshNow()
						})
					}()
					return
				}

				row, okRow := byLabel[selectedLabel]
				if !okRow {
					dialog.ShowError(errors.New("pick something to clone from"), w)
					return
				}
				// The pool the clones land in — the source's parent, which is
				// also where a generated name has to be unique.
				parent := ""
				if row.DS != nil {
					parent = row.DS.Name
					if i := strings.LastIndexByte(parent, '/'); i >= 0 {
						parent = parent[:i]
					}
				}

				// resolveNames decides and checks EVERY name before the first
				// machine is made. Discovering the collision on clone 7 of 10
				// leaves six machines behind and a half-built batch to clean
				// up by hand.
				//
				// It runs off the UI thread: each check is a virsh and a zfs
				// process (~15ms), so a 64-clone batch is a second of dead
				// window if it happens in the click handler.
				resolveNames := func() ([]string, error) {
					names := make([]string, 0, n)
					if base == "" {
						reserved := make(map[string]bool, n)
						for i := 0; i < n; i++ {
							nm, err := freshName("clone-", reserved, parent)
							if err != nil {
								return nil, err
							}
							names = append(names, nm)
						}
						return names, nil
					}
					for i := 1; i <= n; i++ {
						nm := base
						if n > 1 {
							nm = fmt.Sprintf("%s-%d", base, i)
						}
						names = append(names, nm)
					}
					var clash []string
					for _, nm := range names {
						if nameInUse(nm, parent) {
							clash = append(clash, nm)
						}
					}
					if len(clash) > 0 {
						return nil, fmt.Errorf("these names already exist: %s\n\n"+
							"Pick a different name, or leave it blank to generate one.",
							strings.Join(clash, ", "))
					}
					return names, nil
				}

				plan := planClone
				if hasGolden(row) {
					plan = planCloneGolden
				}
				// Read on the UI thread and passed in: touching a widget from
				// the worker below is a data race, not a shortcut.
				powerOn := startAfter.Checked

				// Sequential on purpose. Each clone is a ZFS clone plus a
				// domain define; running N at once fights libvirt for the
				// same locks and turns a fast, boring operation into a race.
				go func() {
					names, err := resolveNames()
					if err != nil {
						e := err
						fyne.Do(func() { dialog.ShowError(e, w) })
						return
					}
					made, failed, started := 0, 0, 0
					for _, nm := range names {
						p, err := plan(row, nm)
						if err == nil {
							err = runPlan(p)
						}
						if err != nil {
							failed++
							e := err
							bad := nm
							fyne.Do(func() {
								dialog.ShowError(fmt.Errorf("%s: %w", bad, e), w)
							})
							continue
						}
						// Its own cloud-init identity, before it ever boots.
						// virt-clone carries the SOURCE's seed cdrom across by
						// reference, so without this every clone comes up
						// answering to the name of the machine it came off.
						//
						// Reported and skipped rather than fatal here: this
						// dialog clones from an arbitrary source, which may
						// never have been cloud-init seeded at all, and the
						// batch is the operator's to finish. ReseedClone is a
						// no-op when there is no seed cdrom to replace.
						if rerr := ReseedClone(nm, NewVMSpec{
							User: "admin", Password: "kldload",
						}); rerr != nil {
							e, bad := rerr, nm
							fyne.Do(func() {
								dialog.ShowError(fmt.Errorf(
									"%s was cloned but kept the source's identity: %w",
									bad, e), w)
							})
						}
						// Its own console log, for the same reason: libvirt
						// opens it exclusively, so clones sharing the source's
						// path start one at a time and the rest fail.
						if lerr := RetargetCloneLogs(nm); lerr != nil {
							e, bad := lerr, nm
							fyne.Do(func() {
								dialog.ShowError(fmt.Errorf(
									"%s kept the source's console log: %w", bad, e), w)
							})
						}
						made++
						if powerOn {
							// A fresh clone is defined and shut off; planStart
							// needs only the name and state. A failure to boot
							// is reported but does not abort the batch — the
							// machine exists and can be started by hand, and
							// stopping here would leave the rest unstamped.
							sp, serr := planStart(Row{D: Dom{Name: nm, State: "shut off"}})
							if serr == nil {
								serr = runPlan(sp)
							}
							if serr != nil {
								e, bad := serr, nm
								fyne.Do(func() {
									dialog.ShowError(fmt.Errorf("%s was created but would not start: %w",
										bad, e), w)
								})
							} else {
								started++
							}
						}
						done := made
						up := started
						fyne.Do(func() {
							if guiStatus != nil {
								if powerOn {
									guiStatus(fmt.Sprintf("· cloned %d of %d from %s, %d running",
										done, len(names), row.D.Name, up))
								} else {
									guiStatus(fmt.Sprintf("· cloned %d of %d from %s",
										done, len(names), row.D.Name))
								}
							}
							refreshNow()
						})
					}
					okCount, badCount, upCount := made, failed, started
					fyne.Do(func() {
						refreshNow()
						if guiStatus != nil {
							msg := fmt.Sprintf("· %d clone(s) of %s ready", okCount, row.D.Name)
							if powerOn {
								msg = fmt.Sprintf("· %d clone(s) of %s, %d running",
									okCount, row.D.Name, upCount)
							}
							if badCount > 0 {
								msg += fmt.Sprintf(", %d failed", badCount)
							}
							guiStatus(msg)
						}
					})
				}()
			}, w)
		// Fyne sizes a dialog to its content's minimum, and a list's minimum
		// is one row — which is exactly how this ended up as a rectangle you
		// could not read. The size is set explicitly for that reason.
		d.Resize(fyne.NewSize(820, 640))
		d.Show()
	}
	mStorage := menuButton("Storage", theme.StorageIcon(),
		fyne.NewMenuItem("Snapshot…", snapAct),
		fyne.NewMenuItem("Rollback…", rollbackDialog))
	mConfig := menuButton("Configure", theme.SettingsIcon(),
		fyne.NewMenuItem("vCPU / memory…", specsDialog),
		fyne.NewMenuItem("Resize disk…", resizeDialog),
		fyne.NewMenuItem("Autostart on/off", verb(planAutostart)))
	mBuild := menuButton("Build", theme.ContentAddIcon(),
		fyne.NewMenuItem("New VM…", newVMDialog),
		fyne.NewMenuItem("App / Appliance…", func() { applianceDialog("") }),
		// ONE entry. Clone and EZ Fleet asked the same question and differed
		// only in whether the source already existed, which read as two tools
		// for one job. The source dropdown now covers both.
		fyne.NewMenuItem("Clone / Fleet…", cloneAny),
		fyne.NewMenuItem("Make Golden…", goldenAct))
	mEstate := menuButton("Estate", theme.ComputerIcon(),
		fyne.NewMenuItem("Migrate to host…", soon("Migrate (teleport)", "0.3")))

	// the right-click menu on an estate row: every verb, zero travel —
	// selects the row first so the verbs aim at what you clicked
	rowMenuAt = func(r Row, pos fyne.Position) {
		tree.Select("vm/" + r.D.Name)
		m := widget.NewPopUpMenu(fyne.NewMenu("",
			fyne.NewMenuItem("Start", verb(planStart)),
			fyne.NewMenuItem("Reboot", verb(planReboot)),
			fyne.NewMenuItem("Suspend", verb(planSuspend)),
			fyne.NewMenuItem("Resume", verb(planResume)),
			fyne.NewMenuItem("Shut down", verb(planShutdown)),
			fyne.NewMenuItem("Force off…", verb(planForceOff)),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Snapshot…", snapAct),
			fyne.NewMenuItem("Rollback…", rollbackDialog),
			fyne.NewMenuItem("Clone…", cloneAny),
			fyne.NewMenuItem("Make Golden…", goldenAct),
			fyne.NewMenuItem("Firecracker golden", verb(planFCGolden)),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("vCPU / memory…", specsDialog),
			fyne.NewMenuItem("Resize disk…", resizeDialog),
			fyne.NewMenuItem("Autostart on/off", verb(planAutostart)),
			fyne.NewMenuItemSeparator(),
			// no ellipsis: nothing opens, it deletes
			fyne.NewMenuItem("Delete", verb(planDelete)),
		), w.Canvas())
		m.ShowAtPosition(pos)
	}

	// ── batch bar: verbs over the checked set ────────────────────────────
	// Appears only when ≥1 VM is checked (dot-click). Every verb fires
	// across the set immediately — checking the rows and pressing the
	// button is the deliberate act; a dialog asking again is not.
	checkedRows := func() []Row {
		var rows []Row
		for _, g := range st.groups {
			for _, r := range g.Rows {
				if checked[r.D.Name] {
					rows = append(rows, r)
				}
			}
		}
		return rows
	}
	clearChecked := func() {
		for k := range checked {
			delete(checked, k)
		}
		tree.Refresh()
		refreshBatchBar()
	}
	// batchRun builds each row's plan and runs it — no confirmation step
	// once for the whole set. Per-VM errors are collected, not fatal — one
	// bad VM must not abort the rest of a 40-VM sweep.
	// One batch at a time. Two clicks on Delete before the first sweep had
	// cleared the checkboxes launched two goroutines over the same rows,
	// interleaved: every destroy, undefine and zfs destroy ran twice, the
	// second copy failing, and a "9 failed" dialog for nine VMs that were in
	// fact gone (onyx, 2026-09-04 08:15). The audit log shows each command
	// in duplicate. The second press is dropped, not queued — by the time
	// the first batch finishes the rows it was asked about no longer exist.
	var batchBusy atomic.Bool
	batchRun := func(label string, build func(Row) (verbPlan, error)) {
		rows := checkedRows()
		if len(rows) == 0 {
			return
		}
		if !batchBusy.CompareAndSwap(false, true) {
			return
		}
		fire := func() {
			go func() {
				defer batchBusy.Store(false)
				var failed []string
				for _, r := range rows {
					p, err := build(r)
					if err == nil {
						err = runPlan(p)
					}
					if err != nil {
						failed = append(failed, r.D.Name+": "+err.Error())
					}
				}
				fyne.Do(func() {
					if len(failed) > 0 {
						dialog.ShowError(fmt.Errorf("%s — %d failed:\n%s",
							label, len(failed), strings.Join(failed, "\n")), w)
					}
					clearChecked()
					refreshNow()
				})
			}()
		}
		// No confirm, destructive or not: the operator picked the rows and
		// pressed the button, which is the same two deliberate acts a
		// dialog would have asked for again. Failures still surface, and
		// every command is in the audit log.
		fire()
	}
	batchBar := container.NewHBox()
	refreshBatchBar = func() {
		n := len(checkedRows())
		if n == 0 {
			batchBar.Hide()
			return
		}
		lbl := pageHeading(fmt.Sprintf("%d selected", n), acBrand)
		bStart := widget.NewButtonWithIcon("Start", theme.MediaPlayIcon(),
			func() { batchRun("Start", planStart) })
		bStart.Importance = widget.SuccessImportance
		bReboot := widget.NewButtonWithIcon("Reboot", theme.ViewRefreshIcon(),
			func() { batchRun("Reboot", planReboot) })
		bReboot.Importance = widget.HighImportance
		bStop := widget.NewButtonWithIcon("Shut down", theme.MediaStopIcon(),
			func() { batchRun("Shut down", planShutdown) })
		bStop.Importance = widget.WarningImportance
		bKill := widget.NewButtonWithIcon("Force off", theme.ErrorIcon(),
			func() { batchRun("Force off", planForceOff) })
		bKill.Importance = widget.DangerImportance
		bDel := widget.NewButtonWithIcon("Delete", theme.DeleteIcon(),
			func() { batchRun("Delete", planDelete) })
		bDel.Importance = widget.DangerImportance
		bClear := widget.NewButtonWithIcon("", theme.CancelIcon(), clearChecked)
		batchBar.Objects = []fyne.CanvasObject{
			lbl, bStart, bReboot, bStop, bKill, bDel, bClear}
		batchBar.Refresh()
		batchBar.Show()
	}
	batchBar.Hide()

	// NewPadded around every control: the bare HBox packs buttons at
	// theme padding (~4px) which reads cramped — this doubles the gutters
	// and floats the row off the dossier above it
	pad := func(o fyne.CanvasObject) fyne.CanvasObject {
		return container.NewPadded(o)
	}
	for _, m := range []*widget.Button{mStorage, mConfig, mBuild, mEstate} {
		m.Importance = widget.HighImportance
	}
	// Centred, not flush left: the row used to run off the right edge with
	// no margin left on it, which reads as truncated rather than as a row
	// that ended. The trailing gap is a fixed 20px rather than more theme
	// padding, because it is doing a different job — theme padding scales
	// with the theme, this is a deliberate margin at the end of a row of
	// buttons so the last one is never the last pixel.
	endGap := canvas.NewRectangle(color.Transparent)
	endGap.SetMinSize(fyne.NewSize(20, 1))
	buttons := container.NewPadded(container.NewCenter(container.NewHBox(
		pad(btnStart), pad(btnStop), pad(btnKill),
		widget.NewSeparator(),
		pad(mStorage), pad(mConfig), pad(mBuild), pad(mEstate), endGap)))

	// ── selection → panes ────────────────────────────────────────────────
	// A branch (group header) toggles its own fold; a leaf drives the panes.
	tree.OnSelected = func(uid string) {
		if isBranch(uid) {
			if tree.IsBranchOpen(uid) {
				tree.CloseBranch(uid)
			} else {
				tree.OpenBranch(uid)
			}
			tree.Unselect(uid)
			return
		}
		// A catalog leaf is an action, not a selection: opening the build
		// dialog is the whole point, and leaving it selected would strand
		// the dossier and console panes on a VM that does not exist yet.
		// (The leaf's own tap handler covers mouse clicks; this is the
		// path keyboard navigation takes.)
		if name, ok := strings.CutPrefix(uid, applianceUIDPrefix); ok {
			tree.Unselect(uid)
			if openAppliance != nil {
				openAppliance(name)
			}
			return
		}
		r, ok := rowByUID(uid)
		if !ok {
			return
		}
		st.selName = r.D.Name
		renderDossier(r)
		followConsole(r)
	}

	search := widget.NewEntry()
	search.SetPlaceHolder("filter VMs…")
	search.OnChanged = func(q string) {
		st.filter = q
		rebuildView()
		tree.Refresh()
	}

	// foldOffGroups: on first paint, collapse groups with nothing running —
	// "off doesn't need to be expanded" (operator). Runs once so it never
	// fights a manual toggle later.
	didFold := false
	foldOffGroups := func() {
		for _, g := range viewGroups {
			if _, run := groupStats(g.Label); run == 0 {
				tree.CloseBranch("grp/" + g.Label)
			} else {
				tree.OpenBranch("grp/" + g.Label)
			}
		}
	}

	// ── refresh: estate every 2s, ZFS every 30s (the TUI cadence) ────────
	apply := func(doms []Dom, cpuRaw map[string]uint64, at time.Time) {
		if !st.prevAt.IsZero() {
			st.cpu = cpuPercent(st.prevCPU, cpuRaw, at.Sub(st.prevAt), doms)
		}
		st.prevCPU, st.prevAt = cpuRaw, at
		fcRowsNow = fcRowsCached()
		st.groups = withoutFCGhosts(BuildEstate(doms, st.dss, st.snaps, st.rs, st.ann), fcRowsNow)
		rebuildView()
		tree.Refresh()
		if !didFold && len(viewGroups) > 0 {
			foldOffGroups()
			didFold = true
		}
		// selection follows the DOMAIN across refreshes (Select on the
		// already-selected id is a no-op, so no event loop)
		if st.selName != "" {
			if _, ok := rowByUID("vm/" + st.selName); ok {
				tree.Select("vm/" + st.selName)
			}
		}
		status.SetText(fmt.Sprintf("%d domains · rules: %s · %s · vmxplore %s",
			len(doms), st.rs.Source, at.Format("15:04:05"), versionFull()))
		if r, ok := st.selected(); ok {
			renderDossier(r)
			// The console is attached to a PROCESS, not to a name. A domain
			// that stopped and started again is a new qemu with a new
			// serial pty and a new VNC port, so the standing attachment is
			// a dead pipe over stale pixels — and nothing noticed, because
			// this only ever ran on a click. The operator's workaround was
			// to select another VM and come back.
			//
			// Three things mean "re-follow": a different domain, a
			// different power state, or a cumulative CPU counter that went
			// BACKWARDS. That last one is what catches an off-and-on cycle
			// completed between two polls, where the state reads "running"
			// both times and only the counter reveals that the process
			// behind the name was replaced. A reboot from inside the guest
			// keeps the same qemu, keeps counting up, and correctly does
			// not disturb the attachment.
			restarted := r.D.Name == conLastName && r.D.CPUTimeNs < conCPU
			if r.D.Name != conName || r.D.State != conState || restarted {
				followConsole(r)
			}
			conLastName, conCPU = r.D.Name, r.D.CPUTimeNs
		}
	}
	fetchEstate := func() {
		doms, err := lv.Estate()
		if err != nil {
			fyne.Do(func() { status.SetText("libvirt: " + err.Error()) })
			return
		}
		cpuRaw := make(map[string]uint64, len(doms))
		for _, d := range doms {
			cpuRaw[d.Name] = d.CPUTimeNs
		}
		at := time.Now()
		fyne.Do(func() { apply(doms, cpuRaw, at) })
	}
	fetchZFS := func() {
		ann := LoadAnnotations()
		var dss map[string]*Dataset
		var snaps map[string][]string
		if HasZFS() {
			dss, _ = ListDatasets()
			snaps, _ = ListSnapshots()
		}
		fyne.Do(func() { st.dss, st.snaps, st.ann = dss, snaps, ann })
	}
	refreshNow = func() { go fetchEstate() }
	go func() {
		fetchZFS()
		fetchEstate()
		fast := time.NewTicker(2 * time.Second)
		slow := time.NewTicker(30 * time.Second)
		defer fast.Stop()
		defer slow.Stop()
		for {
			select {
			case <-fast.C:
				fetchEstate()
			case <-slow.C:
				fetchZFS()
			}
		}
	}()

	// ── assembly: the frame ──────────────────────────────────────────────
	// Three card panels floating on the steel-black base (the zxplore
	// layered look), each with its accent heading: ESTATE red (brand),
	// CONSOLE amber (the LED), DETAILS blue.
	// container.NewPadded around every card = the black gutters: the base
	// shows through between panels and around the window edge, uniformly.
	gap := func(o fyne.CanvasObject) fyne.CanvasObject {
		return container.NewPadded(o)
	}
	// Connect: switch hypervisors by re-execing with --connect. A live
	// reconnect would have to rebuild every pane's state; a clean re-exec
	// is simpler and correct — the new window comes up pointed at the host.
	connectBtn := widget.NewButtonWithIcon(target.Host, theme.ComputerIcon(),
		func() {
			e := widget.NewEntry()
			e.SetPlaceHolder("host, user@host, or qemu+ssh://host/system (empty = local)")
			if target.SSHHost != "" {
				e.SetText(target.SSHHost)
			}
			dialog.ShowCustomConfirm("Connect to a hypervisor", "Connect",
				"Cancel", container.NewVBox(
					widget.NewLabel("Drives a remote host over ssh — same key as your shell."),
					e), func(ok bool) {
					if !ok {
						return
					}
					exe, _ := os.Executable()
					argv := []string{exe}
					if d := strings.TrimSpace(e.Text); d != "" {
						argv = append(argv, "--connect", d)
					}
					// hand off: replace this process with one aimed at the host
					_ = reexec(exe, argv)
				}, w)
		})
	estateHead := container.NewBorder(nil, nil,
		heading("ESTATE", acBrand), connectBtn)
	// The dossier lives under the tree, in the estate card, not in a third
	// pane of its own. Until 2026-09-04 it shared a bottom-right pane with
	// the verb row, which took a fifth of the console's height for text
	// that belongs beside the selection it describes; the operator asked
	// for two panes — the estate with its details, the screen with its
	// verbs — and that is the whole layout now. The split is draggable
	// because a dossier is four lines for a fresh clone and twenty for an
	// appliance with three disks and a mesh.
	details := container.NewBorder(heading("DETAILS", acBlue), nil, nil, nil,
		container.NewScroll(dossier))
	estateBody := container.NewVSplit(tree, details)
	estateBody.SetOffset(0.62)
	left := gap(card(container.NewBorder(
		container.NewVBox(estateHead, search, batchBar), nil, nil, nil,
		estateBody)))
	// The console card carries two tabs: Screen, then Serial — the two ways
	// to look at the machine you have, the one that lands first in front.
	// Screen leads because it is where the work happens (it shows the same
	// console plus everything graphical); Serial stays for the guest Screen
	// cannot reach — no video, or a broken X. Building and the toolset live in
	// the estate tree (the Apps and kldload tools branches) and the Build
	// menu; until 2026-09-03 they were also VM, Apps and kldload tabs here,
	// which listed the same things twice (operator: "redundant").
	//
	// The kldload tab still exists, but only while a tool is running or its
	// verb page is up: it is appended when a tool opens and removed when
	// the tool's "back" leaves it, so a session started from the tree stays
	// one click away while it runs, and the bar reads Screen · Serial the
	// rest of the time. Re-launching from the tree would restart the tool
	// (runArgv kills the previous one), which is why the tab has to exist
	// while one is live.
	//
	// The tabs share the console card; ⛶ toggles the card full-window (and
	// back) for real console work.
	screenTab := container.NewTabItem(tabLabel("Screen"), vncHost)
	tabs := container.NewAppTabs(
		screenTab,
		container.NewTabItem(tabLabel("Serial"), consoleHost))
	toolsTab := container.NewTabItem(tabLabel("kldload"), toolsHost)
	toolsOpen := false
	// Select by tab, never by index: the index form silently pointed at
	// whatever had moved into slot 2 the first time these were reordered.
	selectToolsTab = func() {
		if !toolsOpen {
			tabs.Append(toolsTab)
			toolsOpen = true
		}
		tabs.Select(toolsTab)
	}
	hideTools = func() {
		if toolsOpen {
			tabs.Remove(toolsTab)
			toolsOpen = false
		}
		tabs.Select(screenTab)
	}
	selectScreenTab = func() { tabs.Select(screenTab) }
	tabs.Select(screenTab)
	var mainContent fyne.CanvasObject
	consoleCard := cardTight(container.NewBorder(nil, nil, nil, nil, tabs))

	// ⛶ — the console IS the window.
	//
	// One window with one thing in it: the selected tab's content handed over
	// edge to edge, with no estate pane, no details pane, no tab bar, no card
	// border and no window padding. On a 2560x1440 guest that chrome is the
	// difference between reading the screen and squinting at it, and every
	// pixel of it shows controls nobody is using mid-console.
	//
	// The window goes fullscreen from the same toggle, so one key does the
	// whole job — on Wayland too, since 2026-09-04, on the monitor the
	// window is on; see driveWindowFullScreen for the GLFW patch that makes
	// that true and VMX_FULLSCREEN_WINDOW=never for the panes-only form.
	//
	// HISTORY: two designs sat here before this one and both failed the same
	// way — by trying to own the window as well as its contents. The second
	// (b44) inverted the control and polled w.FullScreen() on the theory that
	// the WM owns fullscreen and vmxplore merely follows it. It cannot: read
	// fyne/internal/driver/glfw and w.fullScreen is assigned in exactly two
	// places, SetFullScreen and SetFullScreenSecondary, with no window-state
	// callback anywhere writing it. FullScreen() reports what WE last asked
	// for, never what the compositor did, so the WM's own fullscreen key left
	// the flag false, the poller saw no edge, and the operator got a
	// fullscreen window still showing both panes — "fullscreen in a 3rd
	// window instead of fullscreen in 1 window". The poller could only ever
	// echo our own call back at us.
	//
	// WARN: Escape cannot be the escape — the VNC widget owns the keyboard
	// and forwards it to the guest, so anything that wants Escape (vi, a
	// firmware menu, the editor in the writing appliance) would eat it. The
	// two ways back out are the same chord that got you in (the viewer's
	// TypedShortcut catches it even with the console focused, see vnc.go) and
	// the restore control, which lives in a hover-reveal corner: invisible
	// until the pointer arrives, so it costs no guest pixels for the whole
	// session to be useful for the one click that ends it.
	//
	// There is no paste button and no fullscreen button. Both were icons in
	// the pane's top-right corner and both are gone: paste has always been
	// Ctrl+V (TypedShortcut sends the host clipboard as RFB cut text AND
	// types it as keystrokes, so it lands in a guest with or without a
	// clipboard agent), and fullscreen is a chord (shift+insert by default,
	// VMX_FULLSCREEN_KEY to change it). An icon that duplicates
	// a working key is chrome sitting on top of a guest for the whole
	// session to save one keystroke.
	restoreBtn := widget.NewButtonWithIcon("", theme.ViewRestoreIcon(), func() {
		exitFullScreen()
	})
	restoreBtn.Importance = widget.LowImportance
	// consoleOnly is the whole mechanism: swap the window's content between
	// the two-pane view and the selected tab standing alone, and — where
	// the toolkit can be trusted to do it on the right monitor — put the
	// window itself fullscreen too.
	//
	// It is idempotent and is the ONLY place either half is driven, so the
	// chord, the viewer's own shortcut and the restore corner cannot disagree
	// about what state the window is in.
	driveWindow := driveWindowFullScreen()
	consoleOnly := false
	setConsoleOnly := func(on bool) {
		if on == consoleOnly {
			return
		}
		consoleOnly = on
		// Window first, then content: Fyne lays the new content out against
		// the window it is going into, so doing this the other way round
		// sizes the console for the old frame and then stretches it.
		if driveWindow {
			w.SetFullScreen(on)
		}
		if on {
			// The way back, said out loud for a few seconds and then gone.
			//
			// WHY: entering this mode removes every visible affordance at
			// once — the header that prints the chord, the tab bar, the verb
			// row — so an operator who arrived here by pressing a key they
			// half-remembered has nothing on screen telling them how to
			// leave. The restore control is a hover-reveal corner precisely
			// so it costs no guest pixels, which also makes it invisible to
			// anyone not already looking for it. A banner that names the key
			// and then removes itself pays the pixel cost once instead of for
			// the whole session.
			exitHint := widget.NewLabel(
				fullScreenKeyLabel + " or the top-right corner to go back")
			exitHint.Importance = widget.LowImportance
			hint := container.NewHBox(layout.NewSpacer(), exitHint,
				layout.NewSpacer())
			// SetPadded(false) is the last of the border: Fyne insets window
			// content by a theme margin, which around a guest that should
			// reach the window's edges reads as a stray frame.
			w.SetPadded(false)
			w.SetContent(container.NewStack(
				tabs.Selected().Content,
				container.NewVBox(container.NewHBox(layout.NewSpacer(),
					newHoverReveal(restoreBtn))),
				container.NewVBox(hint)))
			// Captured by value: a second toggle before this fires must not
			// hide the NEXT banner, and leaving the mode drops the container
			// on the floor anyway.
			go func(h *fyne.Container) {
				time.Sleep(4 * time.Second)
				fyne.Do(func() { h.Hide() })
			}(hint)
			return
		}
		w.SetPadded(true) // the window's own margin comes back with the chrome
		w.SetContent(mainContent)
		// the borrowed pane is going back into its tab: Fyne needs telling
		// that the tab's content moved parents and back again
		tabs.Refresh()
	}
	exitFullScreen = func() { setConsoleOnly(false) }
	toggleFullScreen = func() { setConsoleOnly(!consoleOnly) }
	// Two registrations for one key, because there are two worlds it has to
	// work in. The canvas shortcut covers the window at large — the operator
	// is in the estate tree or a dialog and wants the console big. The
	// viewer's own TypedShortcut (see vnc.go) covers the case the canvas
	// cannot: a focused console swallows the keyboard by design, which is
	// exactly when this key is most wanted and would otherwise be forwarded
	// to the guest as an unremarkable keypress.
	w.Canvas().AddShortcut(fullScreenKey, func(fyne.Shortcut) { toggleFullScreen() })
	// ── the manual: the family front page, shipped in the binary ─────────
	//
	// Same shape as zxplore's and wgxplore's so the three read as one
	// product: a full-window page on the base colour, a centred mark with
	// the wordmark stacked beside it, the manual body centred so the
	// fixed-width page floats mid-window rather than hugging the left
	// edge, and a footer that credits the substrate and closes the page.
	// Only the icon and the accent belong to this console.
	//
	// A static binary copied onto a stranger's box must not be
	// undocumented, which is why the page travels inside it (manual.go)
	// rather than depending on a man(1) installation.
	var showManual func()
	// Labelled "Manual", exactly as zxplore and wgxplore label theirs — a
	// bare "?" beside two unlabelled icon buttons reads as a third mystery
	// glyph rather than as the way to the documentation.
	manualBtn := widget.NewButtonWithIcon("Manual", theme.HelpIcon(),
		func() { showManual() })
	// The chord, printed where the console is. It is configurable, it has
	// changed three times, and an operator who does not know the current
	// value has no way to discover it — the key is not a menu item, and
	// guessing wrong sends the guess straight to the guest, which is how
	// Shift+F12 came to open Chrome's debugger instead of doing anything
	// here. Rendered as disabled body text so it reads as a caption rather
	// than a control: it is the header row, not guest pixels, so it costs
	// nothing to leave up for the whole session.
	// Ordinary importance, bold: it was drawn as disabled text and read as
	// greyed-out chrome rather than as the one key worth knowing ("alt +
	// insert should be brighter", operator, 2026-09-04).
	fsHint := widget.NewLabel("⛶ " + fullScreenKeyLabel)
	fsHint.TextStyle = fyne.TextStyle{Bold: true}
	// sysdiag beside Manual: the requirements screen is the answer to "why
	// is this tile grey" and to "what does this host need", which is asked
	// from the console, not from inside the manual where the button used
	// to be alone (operator, 2026-09-04). The manual keeps its own copy.
	sysdiagHeadBtn := widget.NewButtonWithIcon("sysdiag", theme.InfoIcon(),
		func() { showSysdiag(st.visibleRows()) })
	// The verb row sits in the console's header, between the heading and
	// the manual button: the verbs act on the machine whose screen is
	// below them, and a row that used to close the window from underneath
	// now costs the console nothing. Fullscreen is unaffected — it borrows
	// the tab's content, not this header (setConsoleOnly).
	// The verb row rides in an HScroll: as a plain row it made the header
	// 1135 px wide, a Fyne split never lets a pane shrink below its content,
	// and the estate/console divider stopped moving on the same day the row
	// arrived ("the bar between left and right pane doesn't work",
	// operator, 2026-09-04). Scrolled, the header asks for ~400 px and the
	// row scrolls sideways only when the pane is genuinely too narrow.
	consoleHead := container.NewBorder(nil, nil,
		heading("CONSOLE", acGold),
		container.NewHBox(fsHint, sysdiagHeadBtn, manualBtn),
		container.NewHScroll(buttons))
	consolePane := gap(container.NewBorder(consoleHead, nil, nil, nil,
		consoleCard))
	// Defaults measured from the operator's own layout (2026-08-13), not
	// guessed: the estate tree needs about a fifth of the width to show a
	// name, a state and a resource line without truncating, and everything
	// past that is width the console could have used. 0.38 gave the tree
	// nearly half again more than it uses, so every session began by
	// dragging the same splitter to the same place. 0.24 rather than 0.21
	// since the dossier moved in beside the tree: its longest line is a
	// disk path, and at a fifth of a 2560-wide window that line wrapped.
	//
	// Starting positions, not constraints — Fyne remembers neither, so this
	// is what every launch looks like until the operator drags. The console
	// dominates because the screen is the point.
	body := container.NewHSplit(left, consolePane)
	body.SetOffset(0.24)
	mainContent = container.NewBorder(nil, status, nil, nil, gap(body))

	// The manual page lives in manual_ui.go: ~100 lines that need a window
	// and something to go back to, and nothing else this function holds.
	// `back` is a func because fullscreen swaps the window's content
	// wholesale — a captured value would restore a stale layout.
	// sysdiag: the requirements screen (sysdiag_ui.go). It lives with the
	// manual because "what can this host do, and why" is documentation of
	// the host, not a thing to build.
	sysdiagBtn := widget.NewButtonWithIcon("sysdiag", theme.InfoIcon(),
		func() { showSysdiag(st.visibleRows()) })
	manual := newManualUI(w, func() fyne.CanvasObject { return mainContent }, sysdiagBtn)
	showManual = manual.Show
	w.Canvas().SetOnTypedKey(func(e *fyne.KeyEvent) { manual.HandleKey(e) })

	w.SetContent(mainContent)
	w.SetOnClosed(func() { detachConsole(); detachVNC(); stopTool(); lv.Close() })

	// The GNOME light/dark variant may not be resolved while the widgets
	// were built — repaint the hand-colored canvas objects once shortly
	// after show, and again on every theme change (the zxplore fix).
	// AddListener, not the deprecated AddChangeListener: the callback form
	// is guaranteed to run on the app goroutine, so applyPalette no longer
	// needs its own fyne.Do hop and cannot race a repaint.
	a.Settings().AddListener(func(fyne.Settings) { applyPalette() })
	go func() {
		time.Sleep(500 * time.Millisecond)
		fyne.Do(applyPalette)
	}()
	w.ShowAndRun()
}

// toolAccent is the colour language of the tool launcher: green builds,
// blue is storage, gold reads, red destroys, purple demos. Package level so
// the estate tree can paint a tool row before the tools pane exists.
func toolAccent(name string) accentPair {
	switch {
	case strings.HasSuffix(name, "-demo") || name == "bob":
		return acBrand
	case name == "kvm-delete":
		return acRed
	case name == "kvm-snap" || name == "ksnap" || name == "kexport" ||
		name == "kimage" || name == "zxplore" || name == "wgx" ||
		name == "kbe" || name == "kldload-snapshot" || name == "krecovery":
		return acBlue
	case name == "kvm-list" || name == "kst" || name == "kst-dashboard" ||
		name == "kldload-sysdiag" || name == "kldload-doctor" ||
		name == "kldload-console":
		return acGold
	case name == "klab" || name == "kube-cluster" || name == "kspawn" ||
		name == "kvm-create" || name == "kvm-clone" || name == "kvm-win" ||
		name == "kube-init":
		return acGreen
	case name == "shell":
		return acOff // a plain prompt: no verb, no colour to earn
	}
	return acGold
}

// ─── terminal fit ────────────────────────────────────────────────────
// fyne-io/terminal sizes itself by a guessed cell — the MinSize of a
// monospace "M" — while its TextGrid draws rows at the grid's own row
// height, which on this theme is taller. Rows = floor(height / guess)
// then overflow the widget by (real − guess) per row, and the bottom line
// lands below the pane's edge where no amount of resizing brings it back
// (onyx, 2026-09-04). fitTerminal hands the terminal a height that makes
// its row count fit the REAL row height, so the last line is on screen.
type termFit struct{ rowH, cellH float32 }

func (f *termFit) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	rows := float32(math.Floor(float64(size.Height / f.rowH)))
	h := rows*f.cellH + 0.5 // +0.5 so the terminal's own floor lands on rows
	if rows < 1 || h > size.Height {
		h = size.Height
	}
	objs[0].Move(fyne.NewPos(0, 0))
	objs[0].Resize(fyne.NewSize(size.Width, h))
}

func (f *termFit) MinSize(objs []fyne.CanvasObject) fyne.Size { return objs[0].MinSize() }

func fitTerminal(term fyne.CanvasObject) fyne.CanvasObject {
	cell := canvas.NewText("M", color.White)
	cell.TextStyle.Monospace = true
	cellH := float32(math.Round(float64(cell.MinSize().Height)))
	rowH := widget.NewTextGridFromString("M").MinSize().Height
	if rowH < cellH {
		rowH = cellH
	}
	return container.New(&termFit{rowH: rowH, cellH: cellH}, term)
}
