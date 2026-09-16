package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// ensureDirPool must define a missing pool and leave an existing one alone.
// Needs a reachable libvirt; skipped otherwise, loudly.
func TestEnsureDirPool(t *testing.T) {
	uri := "qemu:///system"
	if err := exec.Command("virsh", "--connect", uri, "version").Run(); err != nil {
		t.Skipf("libvirt not reachable at %s: %v — this test DID NOT RUN", uri, err)
	}
	name := "vmx-test-pool-" + time.Now().Format("150405")
	dir := filepath.Join(os.TempDir(), name)
	t.Cleanup(func() {
		_ = exec.Command("virsh", "--connect", uri, "pool-destroy", name).Run()
		_ = exec.Command("virsh", "--connect", uri, "pool-undefine", name).Run()
		_ = os.RemoveAll(dir)
	})
	var msgs []string
	progress := func(m string) { msgs = append(msgs, m) }

	ensureDirPool(uri, name, dir, progress)
	out, err := exec.Command("virsh", "--connect", uri, "pool-info", name).CombinedOutput()
	if err != nil {
		t.Fatalf("pool %s not defined after ensureDirPool: %v\n%s", name, err, out)
	}
	if len(msgs) == 0 {
		t.Fatalf("defining a missing pool reported nothing")
	}

	// second call: the pool exists, so nothing is said and nothing changes
	msgs = nil
	ensureDirPool(uri, name, dir, progress)
	if len(msgs) != 0 {
		t.Fatalf("an existing pool was touched: %v", msgs)
	}
}
