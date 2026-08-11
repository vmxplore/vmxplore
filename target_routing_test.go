package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The split-brain: verbs that hardcoded qemu:///system or a bare `zfs` ran
// against the LOCAL box while the domain lived on a remote one. planClone
// snapshotted there and cloned here. This walks the source so a future edit
// cannot quietly reintroduce it — the bug was invisible in review precisely
// because "virsh -c qemu:///system" looks correct.
func TestNoVerbBypassesTheTarget(t *testing.T) {
	// setup.go is exempt: --setup installs a hypervisor on THIS machine by
	// definition, so a local URI is the correct thing there.
	// remote.go defines the target itself.
	exempt := map[string]bool{"setup.go": true, "remote.go": true}

	bareZFS := regexp.MustCompile(`"zfs",\s*"`)
	hardURI := regexp.MustCompile(`"qemu:///system"`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if exempt[f] || strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := strings.TrimSpace(line)
			// Comments and error-message prose may name the URI.
			if strings.HasPrefix(code, "//") || strings.Contains(code, `\n`) {
				continue
			}
			if bareZFS.MatchString(line) && !strings.Contains(line, "zfsArgv") {
				t.Errorf(`%s:%d runs zfs directly — use zfsArgv() so it lands on the hypervisor:
  %s`, f, i+1, code)
			}
			if hardURI.MatchString(line) {
				t.Errorf(`%s:%d hardcodes qemu:///system — use virsh(), virshOut() or target.LibvirtURI:
  %s`, f, i+1, code)
			}
		}
	}
}
