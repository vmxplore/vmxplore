//go:build gui

package main

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
)

func TestVMDisplayName(t *testing.T) {
	cases := map[string]string{
		"app-plex-on-zf":     "Plex on ZFS",  // a catalog build: its catalog name
		"app-adguard-ho":     "AdGuard Home", // ditto
		"app-made-up-x":      "made up x",    // app- but not in the catalog
		"fiend":              "fiend",        // everything else as libvirt names it
		"klab-golden-fedora": "klab-golden-fedora",
	}
	for in, want := range cases {
		if got := vmDisplayName(in); got != want {
			t.Errorf("vmDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFitText(t *testing.T) {
	test.NewApp()
	size, st := theme.TextSize(), fyne.TextStyle{}
	s := "a long catalog description that will not fit in a narrow tree row"
	full := fyne.MeasureText(s, size, st).Width
	if got := fitText(s, full+1, size, st); got != s {
		t.Errorf("fits: got %q, want it untouched", got)
	}
	for _, w := range []float32{full / 2, full / 4, 40} {
		got := fitText(s, w, size, st)
		if !strings.HasSuffix(got, "…") || fyne.MeasureText(got, size, st).Width > w {
			t.Errorf("w=%.0f: got %q (%.0f wide), want a trimmed string that fits", w, got, fyne.MeasureText(got, size, st).Width)
		}
		// as long as possible: one more rune would not fit
		n := len([]rune(strings.TrimSuffix(got, "…")))
		longer := strings.TrimRight(string([]rune(s)[:n+1]), " ") + "…"
		if fyne.MeasureText(longer, size, st).Width <= w && !strings.HasSuffix(string([]rune(s)[:n+1]), " ") {
			t.Errorf("w=%.0f: %q is not the longest fit; %q also fits", w, got, longer)
		}
	}
	if got := fitText(s, 0, size, st); got != "" {
		t.Errorf("w=0: got %q, want empty", got)
	}
}
