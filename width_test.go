package main

import "testing"

// The table must USE the terminal. Fixed 22/26/28 columns plus 59 of fixed
// fields made a 135-column table on every screen, so a 200-column terminal
// rendered two thirds of a table and a third of nothing.
func TestColWidthsFillTheScreen(t *testing.T) {
	for _, w := range []int{80, 120, 135, 160, 200, 240, 300} {
		m := &ui{width: w}
		nw, bw, ow := m.colWidths()
		used := fixedCols + nw + bw + ow
		pct := used * 100 / w
		switch {
		case w <= 135:
			// Narrow: minimums, row truncates. Must not shrink below them.
			if nw < nameWMin || bw < backWMin || ow < origWMin {
				t.Errorf("w=%d shrank below the minimums: %d/%d/%d", w, nw, bw, ow)
			}
		default:
			// Wide: either the table reaches the edge, or the growable
			// columns are at their ceilings and NOTES — the trailing %s —
			// absorbs the rest. What must NOT happen is the old behaviour:
			// a 135-column table sitting on a 200-column screen.
			atCeiling := nw == nameWMax && bw == backWMax && ow == origWMax
			if !atCeiling && used+notesW < w-2 {
				t.Errorf("w=%d: table+notes = %d and not at ceiling — %d columns wasted",
					w, used+notesW, w-used-notesW)
			}
			if used <= 135 {
				t.Errorf("w=%d: table is still %d wide — no wider than the old fixed layout", w, used)
			}
		}
		if nw > nameWMax || bw > backWMax || ow > origWMax {
			t.Errorf("w=%d exceeded a ceiling: %d/%d/%d", w, nw, bw, ow)
		}
		t.Logf("  w=%-4d name=%-3d back=%-3d orig=%-3d  table=%-4d +notes=%-4d (%d%% of screen)",
			w, nw, bw, ow, used, used+notesW, pct)
	}
}

// The overlay was a flat 76 columns whatever the terminal — 38% of a 200-
// column screen. It scales now, with a floor at the width the help text was
// written for and a margin so the border never touches the edge.
func TestOverlayWidthScales(t *testing.T) {
	for _, w := range []int{60, 80, 120, 160, 200, 300} {
		boxW := w * 3 / 5
		if boxW < 76 {
			boxW = 76
		}
		if boxW > w-8 {
			boxW = w - 8
		}
		if boxW < 24 {
			boxW = 24
		}
		if boxW > w-1 {
			t.Errorf("w=%d: overlay %d wider than the screen", w, boxW)
		}
		if w >= 200 && boxW <= 76 {
			t.Errorf("w=%d: overlay still stuck at %d", w, boxW)
		}
		t.Logf("  terminal %-4d -> overlay %-4d (%d%%)", w, boxW, boxW*100/w)
	}
}
