//go:build gui

// gui_vmcard.go — the details pane as a card for the selected VM.
//
// Why: the pane under the estate tree was a monospace dump that started with
// the libvirt name and a column of "key value" lines (operator, 2026-10-01:
// "some sort of tile or nice pane per vm would be nice"). The card leads with
// what a person looks for — which machine, is it up, how to reach it, how big
// it is — and keeps the full dump, unchanged, under "Technical details", so
// nothing that was in the pane is gone.
//
// set() is called on every selection AND on the 2 s estate refresh for the
// selected VM, so it only rewrites text and colours; it never rebuilds
// widgets.

package main

import (
	"fmt"
	"image/color"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

type vmCard struct {
	root        fyne.CanvasObject // what the details pane shows
	placeholder fyne.CanvasObject
	body        fyne.CanvasObject

	dot      *canvas.Circle
	name     *canvas.Text
	sub      *canvas.Text
	pillBg   *canvas.Rectangle
	pillText *canvas.Text

	keys []*canvas.Text
	vals []*canvas.Text
}

// The facts, in the order a person asks them.
var vmCardFacts = []string{"Address", "CPU", "Memory", "Disk", "Snapshots", "Autostart"}

func newVMCard(technical fyne.CanvasObject) *vmCard {
	c := &vmCard{
		dot:      canvas.NewCircle(acOff.at()),
		name:     canvas.NewText("", brightFg()),
		sub:      canvas.NewText("", tileSubColor()),
		pillBg:   canvas.NewRectangle(acOff.at()),
		pillText: canvas.NewText("", color.White),
	}
	c.name.TextStyle = fyne.TextStyle{Bold: true}
	c.name.TextSize = theme.TextSize() * 1.45
	c.sub.TextSize = theme.TextSize() * 0.9
	c.pillBg.CornerRadius = 9
	c.pillText.TextStyle = fyne.TextStyle{Bold: true}
	c.pillText.TextSize = theme.TextSize() * 0.85

	dotBox := container.NewGridWrap(fyne.NewSize(14, 14), c.dot)
	pill := container.NewStack(c.pillBg, container.NewPadded(c.pillText))
	header := container.NewBorder(nil, nil,
		container.NewHBox(container.NewCenter(dotBox), gapX(10)),
		container.NewCenter(pill),
		container.NewVBox(c.name, c.sub))

	form := container.New(layout.NewFormLayout())
	for _, k := range vmCardFacts {
		kt := canvas.NewText(k, tileSubColor())
		vt := canvas.NewText("", brightFg())
		c.keys = append(c.keys, kt)
		c.vals = append(c.vals, vt)
		form.Add(kt)
		form.Add(vt)
	}

	tech := widget.NewAccordion(widget.NewAccordionItem("Technical details", technical))
	c.body = container.NewVBox(header, widget.NewSeparator(), form, tech)
	c.placeholder = container.NewCenter(canvas.NewText("Select a VM to see its details", tileSubColor()))
	c.root = container.NewStack(c.placeholder, container.NewVScroll(container.NewPadded(c.body)))
	c.root.(*fyne.Container).Objects[1].Hide()
	return c
}

// set paints the card for r. cpu is the live percentage (or <0 when there
// is none), group the estate group the row sits in ("" if none).
func (c *vmCard) set(r Row, cpu float64, group string) {
	c.placeholder.Hide()
	c.root.(*fyne.Container).Objects[1].Show()

	state, pill, col := strings.ToUpper(r.D.State[:1])+r.D.State[1:], "", acOff.at()
	switch {
	case r.Synthetic || len(r.Notes) > 0:
		pill, col = "Attention", acGold.at()
	case r.D.State == "running":
		pill, col = "Running", acGreen.at()
	case r.D.State == "shut off":
		pill = "Off"
	default:
		pill, col = state, brightFg()
	}
	c.dot.FillColor = col
	c.pillBg.FillColor = col
	c.pillText.Text = pill
	c.pillText.Color = color.White
	if pill == "Running" || pill == "Attention" {
		// dark text: white on the bright green/gold pills was hard to read
		c.pillText.Color = color.NRGBA{R: 0x0b, G: 0x14, B: 0x10, A: 0xff}
	}
	if pill == "Off" {
		// a stopped machine is not a warning: neutral pill, not the
		// dormant-brown the tree uses for its dot
		c.pillBg.FillColor = tileColor()
		c.pillText.Color = tileSubColor()
	}

	c.name.Text = vmDisplayName(r.D.Name)
	c.name.Color = brightFg()
	var sub []string
	if c.name.Text != r.D.Name {
		sub = append(sub, r.D.Name)
	}
	if group != "" {
		sub = append(sub, group)
	}
	if r.FC != nil {
		sub = append(sub, "Firecracker microVM")
	}
	c.sub.Text = strings.Join(sub, "  ·  ")
	c.sub.Color = tileSubColor()

	addr := firstIPv4(r.D.IPs)
	if r.FC != nil && r.FC.IP != "" {
		addr = r.FC.IP
	}
	switch {
	case addr != "":
	case r.D.State == "running" && !r.D.AgentUp:
		addr = "unknown (no guest agent)"
	case r.D.State == "running":
		addr = "waiting for an address"
	default:
		addr = "—"
	}
	vcpus, mem := int(r.D.VCPUs), memCell(r)
	if r.FC != nil {
		vcpus, mem = r.FC.VCPUs, fmt.Sprintf("%d MB", r.FC.RAMMB)
	}
	cpuS := fmt.Sprintf("%d vCPU", vcpus)
	if cpu >= 0 && r.D.State == "running" {
		cpuS += fmt.Sprintf("  ·  %.0f%% busy", cpu)
	}
	disk, snaps := "—", "—"
	if r.DS != nil {
		disk = humanBytes(r.DS.Used) + " used"
		if r.Origin != "" {
			disk += "  ·  a clone"
		}
		if r.SnapTotal > 0 {
			snaps = fmt.Sprintf("%d", r.SnapTotal)
		} else {
			snaps = "none"
		}
	}
	auto := "no"
	if r.D.Autostart {
		auto = "yes"
	}
	vals := []string{addr, cpuS, cellOr(mem, "—"), disk, snaps, auto}
	for i, v := range vals {
		c.vals[i].Text = v
		c.vals[i].Color = brightFg()
		c.keys[i].Color = tileSubColor()
	}
	c.root.Refresh()
}

// clear goes back to the placeholder (nothing selected).
func (c *vmCard) clear() {
	c.root.(*fyne.Container).Objects[1].Hide()
	c.placeholder.Show()
	c.root.Refresh()
}

// gapX is fixed horizontal space; layout.NewSpacer collapses to nothing
// inside a Border's side slot, and the dot sat against the name.
func gapX(w float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(w, 1))
	return r
}
