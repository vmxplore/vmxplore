// build_all_test.go — the arithmetic behind parallel build-all, and the
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
		name              string
		n, maxTile, avail int
		env               string
		want              int
	}{
		{"onyx today: 10 tiles, 2G max, 21G free", 10, 2048, 21504, "", 8},
		{"everything fits", 10, 2048, 32768, "", 10},
		{"tight host serialises", 10, 2048, 5000, "", 1},
		{"no memory reading at all", 10, 2048, 0, "", 1},
		{"env override wins", 10, 2048, 5000, "4", 4},
		{"env override capped at n", 3, 2048, 5000, "9", 3},
		{"garbage env ignored", 10, 2048, 21504, "lots", 8},
		{"zero env ignored", 10, 2048, 21504, "0", 8},
		{"nothing to build", 0, 2048, 21504, "", 1},
	}
	for _, c := range cases {
		if got := buildJobs(c.n, c.maxTile, c.avail, c.env); got != c.want {
			t.Errorf("%s: buildJobs(%d,%d,%d,%q) = %d, want %d",
				c.name, c.n, c.maxTile, c.avail, c.env, got, c.want)
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
	a := Appliance{Name: "Thing", Fields: []ApplianceField{
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
		"Thing  (app-thing)", "http://192.0.2.9:8080/",
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
