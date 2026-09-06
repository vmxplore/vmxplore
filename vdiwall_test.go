package main

import (
	"strings"
	"testing"
)

// The wall picks VDI rows only, walks sessions until the first miss, and
// renders one tile per stream with the muted autoplay player and a link to
// the full page.
func TestVDIWall(t *testing.T) {
	rows := []Row{
		{D: Dom{Name: "app-vdi-deskto", State: "running", IPs: []string{"192.168.122.34"}}},
		{D: Dom{Name: "app-web-stack", State: "running", IPs: []string{"192.168.122.146"}}},
		{D: Dom{Name: "app-vdi-deskto-2", State: "shut off", IPs: []string{"192.168.122.99"}}},
		{D: Dom{Name: "vdi-stack-1", State: "running", IPs: []string{"192.168.122.201"}}, FC: &FCInstance{Golden: "app-vdi-deskto"}},
		{D: Dom{Name: "lamp-stack-4", State: "running", IPs: []string{"192.168.122.216"}}, FC: &FCInstance{Golden: "app-lamp-stack"}},
	}
	// .34 streams two sessions, the microVM one; a web stack must never be asked
	probe := func(u string) bool {
		switch u {
		case "http://192.168.122.34:8889/session1/", "http://192.168.122.34:8889/session2/",
			"http://192.168.122.201:8889/session1/":
			return true
		}
		if strings.Contains(u, "122.146") || strings.Contains(u, "122.216") || strings.Contains(u, "122.99") {
			t.Errorf("probed a non-VDI or shut-off host: %s", u)
		}
		return false
	}
	got := VDIWallStreams(rows, probe)
	if len(got) != 3 {
		t.Fatalf("want 3 streams, got %d: %+v", len(got), got)
	}
	if got[0].Host != "app-vdi-deskto" || got[0].Session != 1 || got[1].Session != 2 || got[2].Host != "vdi-stack-1" {
		t.Errorf("order or selection wrong: %+v", got)
	}
	page := VDIWallHTML(got)
	for _, want := range []string{
		`src="http://192.168.122.34:8889/session2/?autoplay=true&amp;muted=true&amp;controls=false"`,
		`href="http://192.168.122.201:8889/session1/"`,
		`allow="autoplay"`,
		`--cols:2`,
		`3 desktops`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if empty := VDIWallHTML(nil); !strings.Contains(empty, "No VDI desktop is streaming") {
		t.Error("an empty wall must say so")
	}
}
