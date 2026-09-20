package main

import "testing"

// wantLightTheme decides which palette the TUI installs, and getting it
// backwards is not a cosmetic bug: the light palette on a dark terminal
// measures 1.9:1 to 4.4:1 against the background, under the 4.5:1 floor for
// body text, so the whole console becomes unreadable mud. That is what
// shipped, because the old one-liner read "light unless proven dark" and the
// terminal query goes unanswered over plenty of ssh sessions.
//
// So the decision table is pinned here. The asymmetry is the point: light is
// taken only when something says so outright.
func TestWantLightTheme(t *testing.T) {
	cases := []struct {
		name, vmx, fgbg string
		wantLight       bool
	}{
		{"explicit dark wins", "dark", "0;15", false},
		{"explicit light wins", "light", "15;0", true},
		{"COLORFGBG dark bg 0", "", "15;0", false},
		{"COLORFGBG light bg 15", "", "0;15", true},
		{"COLORFGBG light bg 7", "", "0;7", true},
		{"COLORFGBG dark bg 8", "", "15;8", false},
		{"COLORFGBG 3 fields", "", "15;default;0", false},
	}
	for _, c := range cases {
		t.Setenv("VMX_THEME", c.vmx)
		t.Setenv("COLORFGBG", c.fgbg)
		if got := wantLightTheme(); got != c.wantLight {
			t.Errorf("%s: VMX_THEME=%q COLORFGBG=%q -> light=%v, want %v",
				c.name, c.vmx, c.fgbg, got, c.wantLight)
		} else {
			t.Logf("ok  %-24s light=%v", c.name, got)
		}
	}
}
