package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Undos run newest-first. This is the ordering the transient domain relies
// on: it must be torn down before the zvol it has open, or zfs destroy
// fails with "dataset is busy" and both leak.
func TestRollbackUnwindsNewestFirst(t *testing.T) {
	var order []string
	rb := &rollback{}
	rb.add(func() { order = append(order, "disk") })
	rb.add(func() { order = append(order, "seed") })
	rb.add(func() { order = append(order, "domain") })
	rb.run(func(string) {})

	got := strings.Join(order, ",")
	if got != "domain,seed,disk" {
		t.Errorf("unwind order = %s, want domain,seed,disk", got)
	}
}

// A committed build owns its resources. Running cleanup then would delete
// the VM that was just successfully created.
func TestCommitDisarms(t *testing.T) {
	ran := false
	rb := &rollback{}
	rb.add(func() { ran = true })
	rb.commit()
	rb.run(func(string) {})
	if ran {
		t.Error("a committed build must not unwind")
	}
}

// run is deferred unconditionally and may also be reached explicitly; doing
// the work twice would, for instance, destroy a dataset a retry just made.
func TestRunIsIdempotent(t *testing.T) {
	n := 0
	rb := &rollback{}
	rb.add(func() { n++ })
	rb.run(func(string) {})
	rb.run(func(string) {})
	if n != 1 {
		t.Errorf("undo ran %d times, want exactly 1", n)
	}
}

// The guard that stops the whole mechanism from being dangerous: a build
// must never register a delete for a file it did not create.
func TestFileAbsentDistinguishesOurFilesFromTheirs(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "new.qcow2")
	if !fileAbsent(mine) {
		t.Error("a path that does not exist must report absent")
	}
	theirs := filepath.Join(dir, "existing.qcow2")
	if err := os.WriteFile(theirs, []byte("someone else's disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fileAbsent(theirs) {
		t.Error("an existing disk must NOT be reported absent — it would be registered for deletion")
	}
}
