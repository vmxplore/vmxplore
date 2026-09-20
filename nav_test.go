package main

import "testing"

// pgup/pgdown/home/end must land in range for every window size and list
// length, including the degenerate ones: a window too short for a page, an
// empty list, and a cursor already at an end. An off-by-one here indexes
// navItems() out of bounds and panics the whole TUI.
func TestPagingStaysInRange(t *testing.T) {
	for _, h := range []int{0, 1, 6, 7, 24, 50} {
		for _, n := range []int{0, 1, 5, 40} {
			m := &ui{height: h}
			ps := m.pageSize()
			if ps < 3 {
				t.Fatalf("height=%d pageSize=%d, must never be under 3", h, ps)
			}
			for _, start := range []int{0, n / 2, max(0, n-1)} {
				// pgdown — clamped at 0 because an empty list gives n-1 == -1,
				// and a cursor of -1 indexes items[-1] and panics.
				got := max(0, min(n-1, start+ps))
				if got < 0 || (n > 0 && got > n-1) {
					t.Errorf("h=%d n=%d start=%d pgdown -> %d out of [0,%d]", h, n, start, got, n-1)
				}
				// pgup
				up := max(0, start-ps)
				if up < 0 || (n > 0 && up > n-1) {
					t.Errorf("h=%d n=%d start=%d pgup -> %d out of [0,%d]", h, n, start, up, n-1)
				}
				// end
				end := max(0, n-1)
				if end < 0 {
					t.Errorf("h=%d n=%d end -> %d", h, n, end)
				}
			}
		}
	}
}
