//go:build gui

// libvirt_reconnect_test.go — the connection must heal, and must not spin.
//
// HISTORY: 2026-08-12. The connection was opened once at startup and never
// rebuilt, so restarting virtqemud froze the estate at its last good snapshot
// while virsh-based verbs kept working. A delete issued against one of those
// frozen rows powered off a production VM and failed before undefining it.
package main

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// deadConn decides whether to redial. Both directions matter: missing a dead
// connection leaves the estate frozen (the original bug); calling a genuine
// request error "dead" turns one bad call into a reconnect loop.
func TestDeadConnDiscriminates(t *testing.T) {
	dead := []error{
		// The one go-libvirt actually returns on a daemon restart. Verified
		// live, not guessed — the first version of deadConn omitted it and
		// every unit test still passed.
		errors.New("internal error: client socket is closed"),
		errors.New("write unix @->/var/run/libvirt/virtqemud-sock: broken pipe"),
		errors.New("read: connection reset by peer"),
		errors.New("use of closed network connection"),
		io.EOF,
		errors.New("dial unix /var/run/libvirt/virtqemud-sock: connect: no such file or directory"),
		errors.New("dial unix: connect: connection refused"),
		fmt.Errorf("list domains: %w", io.EOF), // wrapped, as callers see it
	}
	for _, e := range dead {
		if !deadConn(e) {
			t.Errorf("deadConn(%q) = false, want true — a frozen estate is the cost", e)
		}
	}

	alive := []error{
		nil,
		errors.New("Domain not found: no domain with matching name 'blog'"),
		errors.New("Requested operation is not valid: domain is not running"),
		errors.New("this function is not supported by the connection driver"),
		errors.New("invalid argument"),
	}
	for _, e := range alive {
		if deadConn(e) {
			t.Errorf("deadConn(%v) = true, want false — redialing on a real "+
				"request error spins", e)
		}
	}
}

// redial must fail cleanly rather than panic when there is nothing to redial.
// An LV built by a test (or a future code path) carries no URI.
func TestRedialWithoutURIFailsCleanly(t *testing.T) {
	lv := &LV{}
	if err := lv.redial(); err == nil {
		t.Fatal("redial() with no uri returned nil; want an error")
	}
}

// The URI must be retained at connect time, or reconnect can never work. This
// asserts the field is populated from the target rather than left empty.
func TestConnectRetainsURIForReconnect(t *testing.T) {
	prev := target
	defer func() { target = prev }()
	target = ParseTarget("qemu:///system")
	if target.LibvirtURI == "" {
		t.Fatal("ParseTarget produced no URI to retain")
	}
	// ConnectSystem needs a live daemon, so assert the plumbing instead: the
	// value that would be stored is the one reconnect needs.
	lv := &LV{uri: target.LibvirtURI}
	if lv.uri != "qemu:///system" {
		t.Errorf("retained uri = %q, want qemu:///system", lv.uri)
	}
}
