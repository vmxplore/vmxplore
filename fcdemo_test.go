package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The demo is three goldens; a host missing any of them must be told which,
// not handed two thirds of a wall.
func TestDemoMissingGoldens(t *testing.T) {
	all := []string{"app-vdi-deskto", "app-rdp-deskto", "app-lamp-stack", "app-web-stack"}
	if got := DemoMissingGoldens(all); len(got) != 0 {
		t.Fatalf("complete host reported missing: %v", got)
	}
	if got := DemoMissingGoldens(nil); len(got) != 3 {
		t.Fatalf("empty host: want 3 missing, got %v", got)
	}
	got := DemoMissingGoldens([]string{"app-lamp-stack"})
	want := []string{"app-vdi-deskto", "app-rdp-deskto"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("missing in lane order: got %v, want %v", got, want)
	}
}

// The lanes must run AT THE SAME TIME — that is the whole reason the tile
// exists rather than three presses of Clone. Three lanes that each sleep
// 200 ms finish in about 200 ms, not 600.
func TestRunDemoLanesIsConcurrent(t *testing.T) {
	lanes := DemoLanes()
	var peak, live int32
	start := time.Now()
	errs := RunDemoLanes(context.Background(), lanes, func(_ context.Context, _ demoLane) error {
		n := atomic.AddInt32(&live, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
		atomic.AddInt32(&live, -1)
		return nil
	})
	if len(errs) != 0 {
		t.Fatalf("clean run reported errors: %v", errs)
	}
	if peak < int32(len(lanes)) {
		t.Errorf("lanes ran %d at a time, want %d — they are serialised", peak, len(lanes))
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("three 200ms lanes took %s — serial, not parallel", d)
	}
}

// One lane failing must not cancel the others, and every failure must be
// named: a demo with the web servers up and the desktops broken is still
// worth looking at, provided the operator is told which half died.
func TestRunDemoLanesReportsEveryFailureAndRunsTheRest(t *testing.T) {
	lanes := DemoLanes()
	var ran int32
	errs := RunDemoLanes(context.Background(), lanes, func(_ context.Context, l demoLane) error {
		atomic.AddInt32(&ran, 1)
		if l.Follow == "rdp" || l.Follow == "browser" {
			return errors.New("boom")
		}
		return nil
	})
	if ran != int32(len(lanes)) {
		t.Errorf("ran %d lanes, want all %d — a failure stopped the others", ran, len(lanes))
	}
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %d: %v", len(errs), errs)
	}
	for _, e := range errs {
		if !strings.Contains(e.Error(), "boom") {
			t.Errorf("error lost its cause: %v", e)
		}
		if !strings.Contains(e.Error(), "app-") {
			t.Errorf("error does not name its golden: %v", e)
		}
	}
}

// Firefox by name, on the operator's call. The fallback is xdg-open, and a
// host with neither gets nothing rather than a wrong guess.
func TestBrowserArgvPrefersFirefox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if got := BrowserArgv("http://x/"); got != nil {
		t.Fatalf("empty PATH should yield no browser, got %v", got)
	}
	writeExec(t, filepath.Join(dir, "xdg-open"))
	got := BrowserArgv("http://x/")
	if len(got) == 0 || !strings.HasSuffix(got[0], "xdg-open") {
		t.Fatalf("with only xdg-open: got %v", got)
	}
	writeExec(t, filepath.Join(dir, "firefox"))
	got = BrowserArgv("http://x/")
	if len(got) != 2 || !strings.HasSuffix(got[0], "firefox") || got[1] != "http://x/" {
		t.Fatalf("firefox must win and carry the target: got %v", got)
	}
}

// THE security property of the auto-login: the password reaches the client
// on stdin and appears nowhere in argv, where ps would show it to every
// user on the host.
func TestRDPLoginArgvKeepsThePasswordOutOfArgv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if argv, _ := RDPLoginArgv("10.0.0.9", "admin"); argv != nil {
		t.Fatalf("no client installed should yield no argv, got %v", argv)
	}
	writeExec(t, filepath.Join(dir, "xfreerdp"))
	argv, wantsStdin := RDPLoginArgv("10.0.0.9", "admin")
	if !wantsStdin {
		t.Error("FreeRDP takes the password on stdin — the caller must be told to write it")
	}
	joined := strings.Join(argv, " ")
	for _, forbidden := range []string{DefaultGuestPassword, "/p:"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("credential material in argv (%q): %s", forbidden, joined)
		}
	}
	for _, want := range []string{"/v:10.0.0.9", "/u:admin", "/from-stdin:force"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %s: %s", want, joined)
		}
	}
}

// Four seats must be sized so four fit one screen, and the size must be
// overridable for a host whose screen is not 1080p.
func TestRDPSeatSizeIsOverridable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	writeExec(t, filepath.Join(dir, "xfreerdp"))
	t.Setenv("VMX_RDP_SIZE", "")
	argv, _ := RDPLoginArgv("10.0.0.1", "admin")
	if argv == nil {
		t.Fatal("no client found")
	}
	// the size is appended by OpenRDPSeats, not RDPLoginArgv; assert the
	// default it uses is a quarter of 1080p
	if got := seatSizeOrDefault(""); got != "960x540" {
		t.Errorf("default seat size %q, want 960x540 (a quarter of 1080p)", got)
	}
	if got := seatSizeOrDefault(" 1280x720 "); got != "1280x720" {
		t.Errorf("override not honoured: %q", got)
	}
}

// A web tile on :80 is a bare URL; anything else carries its port.
func TestDemoWebURL(t *testing.T) {
	for _, tc := range []struct {
		ip, want string
		port     int
	}{
		{"10.0.0.1", "http://10.0.0.1/", 80},
		{"10.0.0.1", "http://10.0.0.1/", 0},
		{"10.0.0.1", "http://10.0.0.1:8096/", 8096},
	} {
		if got := DemoWebURL(tc.ip, tc.port); got != tc.want {
			t.Errorf("DemoWebURL(%s,%d) = %s, want %s", tc.ip, tc.port, got, tc.want)
		}
	}
}

func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The fleet size is the operator's: a 62 GB host runs six of each where a
// 16 GB host runs four, and a nonsense value falls back rather than failing.
func TestDemoCountKnob(t *testing.T) {
	// the file is only consulted when the variable is empty, and this host
	// may have one; point the test at the variable in every case but the
	// explicit fallback below.
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"6", 6}, {"1", 1}, {"17", 17}, {"32", 32},
		{"99", 32}, {"0", DemoCountDefault}, {"-3", DemoCountDefault}, {"lots", DemoCountDefault},
	} {
		t.Setenv("VMX_DEMO_COUNT", tc.env)
		if got := demoCount(); got != tc.want {
			t.Errorf("VMX_DEMO_COUNT=%q → %d, want %d", tc.env, got, tc.want)
		}
		lanes := DemoLanes()
		for _, l := range lanes {
			if l.Count != tc.want {
				t.Errorf("VMX_DEMO_COUNT=%q: lane %s has %d, want %d", tc.env, l.Golden, l.Count, tc.want)
			}
		}
	}
}

// The fleet estimate is a warning, not a gate: it must be quiet when there is
// room, specific when there is not, and silent when it cannot measure.
func TestDemoFleetWarning(t *testing.T) {
	if w := DemoFleetWarning(12, 32000); w != "" {
		t.Errorf("12 machines in 32 GB should not warn: %s", w)
	}
	if w := DemoFleetWarning(51, 30000); w != "" {
		t.Errorf("51 machines in 30 GB fits (%d MB) and should not warn: %s", 51*DemoMachineMB, w)
	}
	w := DemoFleetWarning(96, 8000)
	if w == "" {
		t.Fatal("96 machines in 8 GB must warn")
	}
	for _, want := range []string{"96 machines", "8000 MB free", "VMX_DEMO_COUNT"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q: %s", want, w)
		}
	}
	if w := DemoFleetWarning(40, 0); w != "" {
		t.Errorf("unreadable memory must warn about nothing: %s", w)
	}
}

// Seats shrink as the batch grows, and the override always wins.
func TestSeatSizeScalesWithFleet(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{4, "960x540"}, {6, "640x360"}, {9, "640x360"}, {10, "480x270"}, {17, "480x270"}} {
		if got := seatSizeFor("", tc.n); got != tc.want {
			t.Errorf("%d seats → %s, want %s", tc.n, got, tc.want)
		}
	}
	if got := seatSizeFor("1280x720", 17); got != "1280x720" {
		t.Errorf("override ignored: %s", got)
	}
}

// Tiling is best effort and must never fail the demo: no wmctrl, or no X
// display, is a note rather than an error.
func TestTileRDPSeatsDegradesQuietly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if note := tileRDPSeats([]string{"admin@10.0.0.1"}); !strings.Contains(note, "wmctrl") {
		t.Errorf("without wmctrl the note should name it: %q", note)
	}
	writeExec(t, filepath.Join(dir, "wmctrl"))
	t.Setenv("DISPLAY", "")
	if note := tileRDPSeats([]string{"admin@10.0.0.1"}); !strings.Contains(note, "no X display") {
		t.Errorf("without DISPLAY the note should say so: %q", note)
	}
	// A wmctrl that accepts every move reports a clean tile.
	t.Setenv("DISPLAY", ":0")
	if note := tileRDPSeats([]string{"admin@10.0.0.1", "admin@10.0.0.2"}); note != "" {
		t.Errorf("a working wmctrl should tile silently: %q", note)
	}
}

// A lane must not give up on addresses the instant the clone returns: kfire
// writes each address as it enrolls, and asking one moment too early is how
// fifteen healthy LAMP servers opened no browser tab at all.
func TestDemoFreshWithAddressesWaits(t *testing.T) {
	before := map[string]bool{}
	calls := 0
	// The first two reads have no addresses, the third does — the shape of a
	// batch still being enrolled.
	rows := func() []Row {
		calls++
		mk := func(ip string) Row {
			r := Row{}
			r.D.Name = "lamp-stack-1"
			r.FC = &FCInstance{Golden: "app-lamp-stack"}
			if ip != "" {
				r.D.IPs = []string{ip}
			}
			return r
		}
		if calls < 3 {
			return []Row{mk("")}
		}
		return []Row{mk("10.0.0.5")}
	}
	got := DemoFreshWithAddresses("app-lamp-stack", before, 1, rows, func() {}, 10*time.Second)
	if len(got) != 1 || firstIPv4(got[0].D.IPs) != "10.0.0.5" {
		t.Fatalf("waited but got %+v after %d reads", got, calls)
	}
	if calls < 3 {
		t.Errorf("returned after %d reads — it did not wait", calls)
	}
}

// And it must not wait forever: a deadline returns whatever is there.
func TestDemoFreshWithAddressesGivesUp(t *testing.T) {
	rows := func() []Row {
		r := Row{}
		r.D.Name = "lamp-stack-1"
		r.FC = &FCInstance{Golden: "app-lamp-stack"}
		return []Row{r}
	}
	start := time.Now()
	got := DemoFreshWithAddresses("app-lamp-stack", map[string]bool{}, 1, rows, func() {}, 1500*time.Millisecond)
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("waited %s past a 1.5s deadline", d)
	}
	// newInstancesOf drops rows with no address — which is precisely why a
	// lane that asks too early counts zero machines and opens nothing — so
	// past the deadline the honest answer is an empty slice, and the caller
	// reports the shortfall rather than pretending.
	if len(got) != 0 {
		t.Errorf("past the deadline with no addresses, want none: %+v", got)
	}
}

// The tile must refuse a start it cannot honour, and say every reason at once
// rather than one per attempt.
func TestDemoBlockers(t *testing.T) {
	all := []string{"app-vdi-deskto", "app-rdp-deskto", "app-lamp-stack"}
	if why := DemoBlockers(false, 0, all); len(why) != 0 {
		t.Errorf("a ready host should not be blocked: %v", why)
	}
	why := DemoBlockers(true, 0, all)
	if len(why) != 1 || !strings.Contains(why[0], "teardown") {
		t.Errorf("a running teardown must block: %v", why)
	}
	why = DemoBlockers(false, 12, all)
	if len(why) != 1 || !strings.Contains(why[0], "12 microVM") {
		t.Errorf("a dirty estate must block and say how many: %v", why)
	}
	// every reason at once — an operator fixing one at a time is the worst UI
	why = DemoBlockers(true, 45, []string{"app-lamp-stack"})
	if len(why) != 3 {
		t.Fatalf("want all three reasons, got %d: %v", len(why), why)
	}
	joined := strings.Join(why, " ")
	for _, want := range []string{"teardown", "45 microVM", "app-vdi-deskto", "app-rdp-deskto"} {
		if !strings.Contains(joined, want) {
			t.Errorf("reasons missing %q: %v", want, why)
		}
	}
}

// The demo dialog and tile used to hardcode "4 streamed desktops" and "twelve
// machines at once" while the total beside them was computed from the lanes.
// On the fiend take of 2026-09-07 that put "4" and "twelve" on screen during a
// deploy of forty-five. These assert the strings track the knob.
func TestDemoLaneBreakdownTracksCount(t *testing.T) {
	t.Setenv("VMX_DEMO_COUNT", "15")
	lanes := DemoLanes()
	got := demoLaneBreakdown(lanes)
	for _, want := range []string{"15 streamed desktops", "15 RDP seats", "15 LAMP servers"} {
		if !strings.Contains(got, want) {
			t.Errorf("breakdown %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "4 ") {
		t.Errorf("breakdown still carries a hardcoded 4: %q", got)
	}
}

func TestDemoTileDetailStatesTheRealTotal(t *testing.T) {
	for _, tc := range []struct{ count, total string }{{"15", "45 machines"}, {"4", "12 machines"}} {
		t.Setenv("VMX_DEMO_COUNT", tc.count)
		got := demoTileDetail()
		if !strings.HasPrefix(got, tc.total) {
			t.Errorf("count=%s: tile detail %q does not start with %q", tc.count, got, tc.total)
		}
		if strings.Contains(got, "twelve") {
			t.Errorf("count=%s: tile detail still says 'twelve': %q", tc.count, got)
		}
	}
}
