package main

import (
	"os"
	"strings"
	"testing"
)

// TestDataDiskParentLive runs against the host's real pools (sudo zfs), like
// the other *_live_test.go files: a parent that exists is honoured, one that
// does not is reported and the root parent is used instead.
func TestDataDiskParentLive(t *testing.T) {
	if !HasZFS() {
		t.Skip("no zfs on this host")
	}
	var msgs []string
	prog := func(s string) { msgs = append(msgs, s) }
	root, err := sudoRun(zfsArgv("list", "-H", "-o", "name", "-d", "0")...)
	if err != nil || strings.TrimSpace(root) == "" {
		t.Skip("no pool visible")
	}
	pool := strings.Fields(root)[0]

	t.Setenv("VMX_DATA_PARENT", pool+"/definitely-not-here-"+t.Name())
	if got := dataDiskParent(pool, prog); got != pool {
		t.Fatalf("missing parent: got %q, want the root parent %q", got, pool)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1], "does not exist") {
		t.Fatalf("missing parent was not reported: %v", msgs)
	}

	t.Setenv("VMX_DATA_PARENT", pool)
	if got := dataDiskParent(pool+"/vms", prog); got != pool {
		t.Fatalf("existing parent: got %q, want %q", got, pool)
	}
	if !strings.Contains(msgs[len(msgs)-1], "$VMX_DATA_PARENT") {
		t.Fatalf("source not named: %v", msgs)
	}

	t.Setenv("VMX_DATA_PARENT", "")
	if _, err := os.Stat("/etc/vmxplore/data-parent"); err != nil {
		if got := dataDiskParent(pool+"/vms", prog); got != pool+"/vms" {
			t.Fatalf("unset: got %q, want the root parent", got)
		}
	}
}
