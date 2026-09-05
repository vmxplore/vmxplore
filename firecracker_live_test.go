package main

import (
	"os"
	"testing"
)

// The live path, end to end: kfire on this host, sudo -n from this user,
// JSON to rows to the group the estate shows. Runs only when asked
// (VMX_LIVE=1) on a host with a cloned instance — the unit tests cover
// the mapping; this covers the plumbing that no fake can.
func TestLiveFirecrackerGroup(t *testing.T) {
	if os.Getenv("VMX_LIVE") != "1" {
		t.Skip("VMX_LIVE=1 to run against this host's kfire")
	}
	if !kfireAvailable() {
		t.Skip("kfire not installed")
	}
	fcInvalidate()
	groups := withFirecracker(nil)
	if len(groups) != 1 || groups[0].Label != fcGroupLabel {
		t.Fatalf("groups = %+v, want one %q group (clone an instance first)", groups, fcGroupLabel)
	}
	for _, r := range groups[0].Rows {
		t.Logf("%s %s %v golden=%s", r.D.Name, r.D.State, r.D.IPs, r.FC.Golden)
		if r.FC == nil || r.D.Name == "" || r.FC.Golden == "" {
			t.Errorf("row %+v is incomplete", r)
		}
	}
}
