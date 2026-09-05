// build_all_test.go — the arithmetic behind build-all's pacing and sizing, and the
// osinfo probe that decides whether virt-install is even asked for a
// variant. Both pure enough to run anywhere; the probe skips without
// osinfo-query.
package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBuildJobs(t *testing.T) {
	cases := []struct {
		name string
		n    int
		env  string
		want int
	}{
		{"default is one at a time", 10, "", 1},
		{"env override wins", 10, "4", 4},
		{"env override capped at n", 3, "9", 3},
		{"garbage env is one", 10, "lots", 1},
		{"zero env is one", 10, "0", 1},
		{"nothing to build", 0, "", 1},
	}
	for _, c := range cases {
		if got := buildJobs(c.n, c.env); got != c.want {
			t.Errorf("%s: buildJobs(%d,%q) = %d, want %d", c.name, c.n, c.env, got, c.want)
		}
	}
}

// A serial build borrows the host; a parallel one and a small host do not.
func TestBuildSize(t *testing.T) {
	tile := Appliance{VCPUs: 2, RAMMB: 2048}
	big := Appliance{VCPUs: 4, RAMMB: 4096}
	cases := []struct {
		name            string
		a               Appliance
		jobs, cpus, mem int
		wantCPU, wantMB int
	}{
		{"onyx alone: cores less two capped at 8, 4x RAM", tile, 1, 24, 21504, 8, 8192},
		{"parallel run keeps catalog size", tile, 3, 24, 21504, 2, 2048},
		{"four-core box gives two", tile, 1, 4, 21504, 2, 8192},
		{"two-core box never goes below catalog", tile, 1, 2, 21504, 2, 8192},
		{"RAM boost capped at what the host can spare", tile, 1, 24, 7000, 8, 2904},
		{"a host with nothing spare builds at catalog size", tile, 1, 24, 5000, 8, 2048},
		{"no memory reading builds at catalog size", tile, 1, 24, 0, 8, 2048},
		{"a big tile hits the 8G cap", big, 1, 24, 32768, 8, 8192},
	}
	for _, c := range cases {
		gotCPU, gotMB := buildSize(c.a, c.jobs, c.cpus, c.mem)
		if gotCPU != c.wantCPU || gotMB != c.wantMB {
			t.Errorf("%s: buildSize(jobs=%d,cpus=%d,mem=%d) = %d/%d, want %d/%d",
				c.name, c.jobs, c.cpus, c.mem, gotCPU, gotMB, c.wantCPU, c.wantMB)
		}
	}
}

func TestOsinfoKnows(t *testing.T) {
	if _, err := exec.LookPath("osinfo-query"); err != nil {
		t.Skip("osinfo-query not installed")
	}
	// Every osinfo-db has generic linux; no osinfo-db has this string.
	if !osinfoKnows("linux2022") && !osinfoKnows("linux2020") {
		t.Error("a generic linux variant must be known")
	}
	if osinfoKnows("no-such-os-variant-xyz") {
		t.Error("an unknown variant must not be reported as known")
	}
}

// The closing report names the URL, the guest login and every secret the
// recipe was given, and nothing that is not a secret.
func TestApplianceAccess(t *testing.T) {
	a := Appliance{Name: "Thing", Summary: "a thing that does things", Fields: []ApplianceField{
		{Key: "T_POOL", Label: "pool name"},
		{Key: "T_DB_PASS", Label: "database password", Secret: true, Generate: true},
		{Key: "T_ADMIN_PASS", Label: "admin password", Secret: true},
	}}
	vals := map[string]string{"T_POOL": "tank", "T_DB_PASS": "s3cr3t", "T_ADMIN_PASS": "hunter2"}
	spec := NewVMSpec{Name: "app-thing", User: "admin", RootSSHKeys: []string{"ssh-ed25519 AAA ops"}}
	lines := strings.Join(applianceAccess(a, spec, vals, "http://192.0.2.9:8080/"), "\n")
	// A tile with a landing line gets it filled in, not the probe URL.
	landing := a
	landing.LandsOn = "http://<vm-ip>:8889/session1 (WebRTC) · rdp://<vm-ip>:3389"
	landing.ClientHint = []string{"sound: xfreerdp /v:<vm-ip> /sound"}
	if l := strings.Join(applianceAccess(landing, spec, vals, "http://192.0.2.9/"), "\n"); !strings.Contains(l, "http://192.0.2.9:8889/session1 (WebRTC) · rdp://192.0.2.9:3389") ||
		!strings.Contains(l, "sound: xfreerdp /v:192.0.2.9 /sound") {
		t.Errorf("landing line or client hint not filled in:\n%s", l)
	}
	for _, want := range []string{
		"Thing  (app-thing)", "a thing that does things", "http://192.0.2.9:8080/",
		"guest login: admin / " + DefaultGuestPassword,
		"root: ssh root@<ip>",
		"database password (T_DB_PASS): s3cr3t",
		"admin password (T_ADMIN_PASS): hunter2",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("report missing %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "tank") {
		t.Errorf("a non-secret field leaked into the login report:\n%s", lines)
	}
	keyOnly := NewVMSpec{Name: "x", User: "admin", SSHKey: "ssh-ed25519 BBB me"}
	if l := strings.Join(applianceAccess(a, keyOnly, vals, "u"), "\n"); !strings.Contains(l, "admin with your ssh key") ||
		strings.Contains(l, DefaultGuestPassword) {
		t.Errorf("key-only guest must not be reported with the default password:\n%s", l)
	}
}

// The URL a finished tile opens is the first real address in its landing
// line, filled in — never the health port when the tile says where it is.
func TestLandingURL(t *testing.T) {
	a := Appliance{LandsOn: "http://<vm-ip>:8889/session1 (WebRTC) · http://<vm-ip>:8888/session1 (HLS)"}
	if got := landingURL(a, "http://192.0.2.9/"); got != "http://192.0.2.9:8889/session1" {
		t.Errorf("landingURL = %q", got)
	}
	rdp := Appliance{LandsOn: "rdp://<vm-ip>:3389 — log in as the guest account"}
	if got := landingURL(rdp, "rdp://192.0.2.9:3389"); got != "rdp://192.0.2.9:3389" {
		t.Errorf("rdp landingURL = %q", got)
	}
	plain := Appliance{LandsOn: "the Screen tab"}
	if got := landingURL(plain, "http://192.0.2.9/"); got != "http://192.0.2.9/" {
		t.Errorf("fallback landingURL = %q", got)
	}
}
