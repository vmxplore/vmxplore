//go:build gui

package main

import "testing"

func TestFitGuestMax(t *testing.T) {
	cases := [][4]int{
		{5120, 2880, 3840, 2160}, // the chopped fullscreen: same 16:9 shape, inside 4K
		{2880, 1800, 2880, 1800}, // already inside: untouched
		{3840, 2160, 3840, 2160},
		{5120, 1440, 3840, 1080}, // ultra-wide: width is the limit
		{2000, 3000, 1440, 2160}, // tall: height is the limit
	}
	for _, c := range cases {
		if w, h := fitGuestMax(c[0], c[1]); w != c[2] || h != c[3] {
			t.Errorf("fitGuestMax(%d,%d) = %dx%d, want %dx%d", c[0], c[1], w, h, c[2], c[3])
		}
	}
}
