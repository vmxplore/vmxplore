//go:build gui

// libvirt_reconnect_live_test.go — prove the heal against a REAL libvirt.
//
// Gated on VMX_RECONNECT_LIVE because it restarts the libvirt daemon, which is
// not something a unit-test run should do to a workstation. Guests keep
// running across the restart; only the management connection drops — which is
// precisely the incident this reproduces.
//
//	sudo VMX_RECONNECT_LIVE=1 go test -tags gui -run LiveReconnect -v .
package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestLiveReconnect(t *testing.T) {
	if os.Getenv("VMX_RECONNECT_LIVE") == "" {
		t.Skip("set VMX_RECONNECT_LIVE=1 (restarts the libvirt daemon)")
	}
	lv, err := ConnectTarget("qemu:///system")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer lv.Close()

	before, err := lv.Estate()
	if err != nil {
		t.Fatalf("Estate() before restart: %v", err)
	}
	t.Logf("before restart: %d domains", len(before))

	// Sever the connection the same way the incident did.
	out, err := exec.Command("systemctl", "restart", "virtqemud").CombinedOutput()
	if err != nil {
		t.Fatalf("restart virtqemud: %v: %s", err, out)
	}

	after, err := lv.Estate()
	if err != nil {
		t.Fatalf("Estate() after restart: %v — the connection did not heal, "+
			"which is the bug: the GUI would keep painting stale state while "+
			"verbs continued to work", err)
	}
	t.Logf("after restart:  %d domains — reconnected", len(after))

	if len(after) != len(before) {
		t.Errorf("domain count changed across the restart: %d -> %d",
			len(before), len(after))
	}
}
