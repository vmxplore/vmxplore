package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneFollowUp(t *testing.T) {
	cases := []struct {
		golden string
		port   int
		want   string
	}{
		{"app-vdi-deskto", 80, "wall"},
		{"app-rdp-deskto", 3389, "rdp"},
		{"my-seat", 3389, "rdp"},
		{"app-web-stack", 80, "browser"},
		{"app-lamp-stack", 80, "browser"},
		{"app-syncthing", 8384, "browser"},
		{"app-mystery", 0, ""},
	}
	for _, c := range cases {
		if got := cloneFollowUp(c.golden, c.port); got != c.want {
			t.Errorf("%s:%d = %q, want %q", c.golden, c.port, got, c.want)
		}
	}
	before := map[string]bool{"lamp-stack-1": true}
	after := []Row{
		{D: Dom{Name: "lamp-stack-1", IPs: []string{"10.0.0.1"}}, FC: &FCInstance{Golden: "app-lamp-stack"}},
		{D: Dom{Name: "lamp-stack-2", IPs: []string{"10.0.0.2"}}, FC: &FCInstance{Golden: "app-lamp-stack"}},
		{D: Dom{Name: "lamp-stack-3"}, FC: &FCInstance{Golden: "app-lamp-stack"}}, // no address yet
		{D: Dom{Name: "web-stack-1", IPs: []string{"10.0.0.9"}}, FC: &FCInstance{Golden: "app-web-stack"}},
	}
	got := newInstancesOf("app-lamp-stack", before, after)
	if len(got) != 1 || got[0].D.Name != "lamp-stack-2" {
		t.Errorf("new instances = %+v, want only lamp-stack-2", got)
	}
}

// The golden argv carries the tile's port so kfire's wait probes the right
// one; a VM this tool did not build gets no --port and kfire's default.
func TestFCGoldenArgs(t *testing.T) {
	if got := fcGoldenArgs("app-rdp-deskto", appliancePortFor("app-rdp-deskto")); len(got) != 4 || got[3] != "3389" {
		t.Errorf("rdp golden args = %v", got)
	}
	if got := fcGoldenArgs("app-lamp-stack", appliancePortFor("app-lamp-stack")); len(got) != 4 || got[3] != "80" {
		t.Errorf("lamp golden args = %v", got)
	}
	if got := fcGoldenArgs("handmade", appliancePortFor("handmade")); len(got) != 2 {
		t.Errorf("unknown VM must get no --port: %v", got)
	}
}

// tab_mode is rewritten in place, added under the section when absent, the
// file created when missing, and left alone when already 3.
func TestRemminaOneWindowPerSeat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "remmina", "remmina.pref")
	changed, err := remminaOneWindowPerSeat(p)
	if err != nil || !changed {
		t.Fatalf("missing file: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "[remmina_pref]\ntab_mode=3") {
		t.Errorf("created file = %q", b)
	}
	os.WriteFile(p, []byte("[remmina_pref]\nfoo=1\ntab_mode=0\nbar=2\n"), 0o600)
	if changed, err = remminaOneWindowPerSeat(p); err != nil || !changed {
		t.Fatalf("rewrite: changed=%v err=%v", changed, err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "[remmina_pref]\nfoo=1\ntab_mode=3\nbar=2\n" {
		t.Errorf("rewritten = %q", b)
	}
	if changed, err = remminaOneWindowPerSeat(p); err != nil || changed {
		t.Errorf("already 3 must be a no-op: changed=%v err=%v", changed, err)
	}
	os.WriteFile(p, []byte("[remmina_pref]\nfoo=1\n"), 0o600)
	remminaOneWindowPerSeat(p)
	b, _ = os.ReadFile(p)
	if string(b) != "[remmina_pref]\ntab_mode=3\nfoo=1\n" {
		t.Errorf("added under section = %q", b)
	}
}
