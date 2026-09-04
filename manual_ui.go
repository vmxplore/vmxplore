//go:build gui

// manual_ui.go — the in-binary manual page, and the keys that read it.
//
// What it does, in order:
//  1. builds a full-window page: mark and wordmark above, the rendered
//     man page centred below, a footer that credits the substrate;
//  2. renders the body asynchronously, because mandoc takes a beat;
//  3. drives its own scroll offset, because container.Scroll answers the
//     wheel and nothing else.
//
// WHY the page travels inside the binary: vmxplore is a static binary people
// copy onto a stranger's box, where `man vmxplore` finds nothing. A console
// that cannot explain itself where it actually runs is undocumented in the
// only place that counts.
//
// WHY this is its own file: it was ~100 lines in the middle of runGUI, which
// is a 2300-line function. Nothing here touches the estate, the console or
// the verbs — it needs a window and something to go back to — so it was one
// of the few pieces that could leave without threading state through a dozen
// closures. See newManualUI's contract for what that "something" is.
//
// Notes: the layout deliberately matches zxplore's and wgxplore's so the
// three read as one product. Only the icon and the accent belong here.
package main

import (
	"image/color"
	"net/url"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// manualUI is the manual page plus the two things a caller needs to drive it.
type manualUI struct {
	// Show puts the page on screen.
	Show func()
	// HandleKey offers a key to the page. It reports whether the page
	// consumed it, which is false whenever the page is closed — so a caller
	// can chain it ahead of its own key handling without a mode flag of its
	// own.
	HandleKey func(*fyne.KeyEvent) bool
}

// newManualUI builds the manual page.
//
// Args:
//
//	w    — the window the page takes over when shown.
//	back — what to restore when the page closes. A FUNC, not a value: the
//	       window's normal content is swapped by other features (fullscreen
//	       replaces it wholesale), so a captured value would restore whatever
//	       happened to be current at build time and undo their work.
//
// Returns: a manualUI. Never fails — an unrenderable man page yields an empty
// body, not an error, because a console must still open its help screen.
//
// Example:
//
//	man := newManualUI(w, func() fyne.CanvasObject { return mainContent })
//	helpBtn.OnTapped = man.Show
//	w.Canvas().SetOnTypedKey(func(e *fyne.KeyEvent) { man.HandleKey(e) })
//
// extras go in the footer beside the site link: controls that belong with
// the documentation rather than the estate — sysdiag lives there, because
// "what can this host do" is a question for the manual, not a tile in the
// Apps catalog (operator, 2026-09-03).
func newManualUI(w fyne.Window, back func() fyne.CanvasObject, extras ...fyne.CanvasObject) *manualUI {
	pageColor := func() color.Color {
		if variantDark() {
			return color.NRGBA{R: 0x08, G: 0x09, B: 0x0c, A: 0xff}
		}
		return color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	}
	pageBG := canvas.NewRectangle(pageColor())
	repaint = append(repaint, func() { pageBG.FillColor = pageColor(); pageBG.Refresh() })
	pageLogo := canvas.NewImageFromResource(
		fyne.NewStaticResource("vmxplore.svg", iconSVG))
	pageLogo.FillMode = canvas.ImageFillContain
	pageLogo.SetMinSize(fyne.NewSize(96, 96))
	pageTitle := heading("v m x p l o r e", acBrand)
	pageTitle.TextSize = 26
	pageSub := heading("the KVM console — vmxplore(1)", acGreen)
	pageSub.TextSize = 13
	pageVer := heading("v"+versionFull(), acGold)
	pageVer.TextSize = 12
	pageHead := container.NewCenter(container.NewHBox(
		pageLogo, container.NewVBox(pageTitle, pageSub, pageVer)))

	// The body renders async: mandoc can take a beat, and a console that
	// stalls on its own help screen is a bad console.
	manBody := widget.NewRichText()
	manBody.Wrapping = fyne.TextWrapOff
	manScroll := container.NewScroll(container.NewCenter(manBody))
	go func() {
		text := renderManual()
		fyne.Do(func() {
			manBody.Segments = manualSegments(text)
			manBody.Refresh()
		})
	}()

	siteU, _ := url.Parse("https://kldload.com")
	powered := widget.NewHyperlink("powered by kldload.com", siteU)
	manClose := widget.NewButtonWithIcon("Close  ⏎", theme.ConfirmIcon(), nil)
	manClose.Importance = widget.HighImportance
	footLeft := container.NewHBox(append([]fyne.CanvasObject{powered}, extras...)...)
	page := container.NewStack(pageBG, container.NewBorder(
		container.NewPadded(pageHead), container.NewPadded(
			container.NewBorder(nil, nil, footLeft, manClose, nil)),
		nil, nil, manScroll))

	open := false
	closeManual := func() {
		open = false
		w.SetContent(back())
	}
	manClose.OnTapped = closeManual

	// Keys for the manual page.
	//
	// A container.Scroll answers the wheel and nothing else — no PgUp, no
	// Home, no arrows — so a 559-line manual could only be read by
	// dragging. Fyne gives no scrolled-text widget that handles them, so
	// the page drives the offset itself.
	//
	// 0.9 of a screen per page keeps two lines of overlap, which is what
	// stops a reader losing their place across a jump.
	scrollBy := func(frac float32) {
		max := manScroll.Content.MinSize().Height - manScroll.Size().Height
		if max < 0 {
			max = 0
		}
		o := manScroll.Offset
		o.Y += manScroll.Size().Height * frac
		if o.Y < 0 {
			o.Y = 0
		}
		if o.Y > max {
			o.Y = max
		}
		manScroll.Offset = o
		manScroll.Refresh()
	}

	return &manualUI{
		Show: func() {
			open = true
			w.SetContent(page)
		},
		HandleKey: func(e *fyne.KeyEvent) bool {
			if !open {
				return false
			}
			switch e.Name {
			case fyne.KeyPageDown, fyne.KeySpace:
				scrollBy(0.9)
			case fyne.KeyPageUp:
				scrollBy(-0.9)
			case fyne.KeyDown:
				scrollBy(0.1)
			case fyne.KeyUp:
				scrollBy(-0.1)
			case fyne.KeyHome:
				manScroll.ScrollToTop()
			case fyne.KeyEnd:
				manScroll.ScrollToBottom()
			case fyne.KeyEscape, fyne.KeyReturn, fyne.KeyEnter:
				closeManual()
			default:
				return false
			}
			return true
		},
	}
}
