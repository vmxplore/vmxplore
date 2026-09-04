//go:build gui

// sysdiag_ui.go — the sysdiag report as a window: a requirements screen.
//
// Top: the detected substrate as a badge, framed in its own colour, beside
// the facts (OS, kernel, CPU, memory, versions). Middle: one card per probe,
// green or red, with the sentence that explains it. Bottom: the three
// substrates side by side — each with its requirements ticked or crossed
// for THIS host, then the feature ladder, with this host's column outlined.
//
// The report itself is sysdiag.go; nothing here probes anything.
package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// tierAccent is the colour of a substrate everywhere the screen names one:
// gold for bare KVM (it works, and every gold row is what the next tier
// adds), blue for KVM + ZFS (storage), green for kldloadOS (everything).
func tierAccent(key string) accentPair {
	switch key {
	case "kldload":
		return acGreen
	case "kvm+zfs":
		return acBlue
	}
	return acGold
}

// framed outlines content in an accent: the badge, and this host's column.
func framed(content fyne.CanvasObject, col color.Color) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.StrokeColor = col
	r.StrokeWidth = 2
	r.CornerRadius = 10
	return container.NewStack(r, container.NewPadded(content))
}

func showSysdiag(rows []Row) {
	d := RunSysdiag(rows)
	acc := tierAccent(d.Tier)
	fg := theme.Color(theme.ColorNameForeground)
	mono := func(s string, col color.Color, bold bool) *canvas.Text {
		t := canvas.NewText(s, col)
		t.TextStyle = fyne.TextStyle{Monospace: true, Bold: bold}
		return t
	}

	// ── badge + facts ──
	badge := canvas.NewText(SysTierLabel(d.Tier), acc.at())
	badge.TextSize = 30
	badge.TextStyle = fyne.TextStyle{Bold: true}
	badgeSub := canvas.NewText("detected substrate", fg)
	badgeSub.TextSize = 11
	badgeBox := framed(container.NewCenter(container.NewVBox(
		container.NewCenter(badge), container.NewCenter(badgeSub))), acc.at())
	factCells := make([]fyne.CanvasObject, 0, 2*len(d.Facts))
	for _, f := range d.Facts {
		factCells = append(factCells, mono(f.Key, acc.at(), true), mono(f.Value, fg, false))
	}
	// two real columns: keys take their own width, values the rest
	keys := container.NewVBox()
	vals := container.NewVBox()
	for i := 0; i < len(factCells); i += 2 {
		keys.Add(factCells[i])
		vals.Add(factCells[i+1])
	}
	facts := container.NewBorder(nil, nil, keys, nil, vals)
	summary := widget.NewLabel(d.Summary)
	summary.Wrapping = fyne.TextWrapWord
	head := container.NewBorder(nil, nil, nil, badgeBox,
		container.NewVBox(pageHeading("SYSDIAG", acGold), summary, card(facts)))

	// ── probes ──
	cards := make([]fyne.CanvasObject, 0, len(d.Probes))
	for _, p := range d.Probes {
		glyph, col := "✓ ", acGreen.at()
		if !p.OK {
			glyph, col = "✗ ", acRed.at()
		}
		t := canvas.NewText(glyph+p.Name, col)
		t.TextStyle = fyne.TextStyle{Bold: true}
		desc := widget.NewLabel(p.Detail)
		desc.Wrapping = fyne.TextWrapWord
		cards = append(cards, card(container.NewVBox(t, desc)))
	}
	probes := container.NewGridWrap(fyne.NewSize(250, 96), cards...)

	// ── requirements: one card per substrate ──
	reqCards := make([]fyne.CanvasObject, 0, len(d.Tiers))
	for _, t := range d.Tiers {
		tc := tierAccent(t.Key)
		title := canvas.NewText(t.Label, tc.at())
		title.TextStyle = fyne.TextStyle{Bold: true}
		title.TextSize = 16
		box := container.NewVBox(title)
		allMet := true
		for _, r := range t.Reqs {
			mark, col := "✓ ", acGreen.at()
			if !r.Met {
				mark, col, allMet = "✗ ", acRed.at(), false
			}
			box.Add(mono(mark+r.Name, col, false))
		}
		verdict, vcol := "requirements met", acGreen.at()
		if !allMet {
			verdict, vcol = "not met on this host", acRed.at()
		}
		v := canvas.NewText(verdict, vcol)
		v.TextStyle = fyne.TextStyle{Italic: true}
		box.Add(v)
		c := card(box)
		if t.Key == d.Tier {
			c = framed(c, tc.at())
		}
		reqCards = append(reqCards, c)
	}
	reqs := container.NewGridWithColumns(len(reqCards), reqCards...)

	// ── the ladder: features by substrate, this host's column outlined ──
	names := container.NewVBox(mono("", fg, false))
	for _, f := range d.Features {
		names.Add(mono(f, fg, false))
	}
	cols := make([]fyne.CanvasObject, 0, len(d.Tiers))
	for _, t := range d.Tiers {
		tc := tierAccent(t.Key)
		col := container.NewVBox(container.NewCenter(mono(t.Label, tc.at(), true)))
		for i := range d.Features {
			mark, mc := "—", fg
			if t.Has[i] {
				mark, mc = "✓", acGreen.at()
			} else if t.Key == d.Tier {
				mc = acGold.at() // what the next tier would add here
			}
			col.Add(container.NewCenter(mono(mark, mc, t.Has[i])))
		}
		var c fyne.CanvasObject = col
		if t.Key == d.Tier {
			c = framed(col, tc.at())
		} else {
			c = container.NewPadded(col)
		}
		cols = append(cols, c)
	}
	ladder := container.NewBorder(nil, nil, container.NewPadded(names), nil,
		container.NewGridWithColumns(len(cols), cols...))

	foot := widget.NewLabel("kldloadOS is a free multi-distro ZFS-on-root installer. " +
		"Same tiles, every column — kldload.com")
	foot.Wrapping = fyne.TextWrapWord
	body := container.NewVBox(
		head,
		heading("PROBES", acBrand), probes,
		heading("REQUIREMENTS", acBrand), reqs,
		heading("CAPABILITIES BY SUBSTRATE", acBrand), card(ladder),
		foot)
	tw := fyne.CurrentApp().NewWindow("sysdiag — " + d.Host)
	tw.SetContent(container.NewVScroll(container.NewPadded(body)))
	tw.Resize(fyne.NewSize(900, 820))
	tw.Show()
}
