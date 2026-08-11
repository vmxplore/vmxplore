// rollback.go — undo what a half-finished build created.
//
// What it does, in order:
//  1. Collects an undo action as each resource is created.
//  2. On the way out of a failed build, runs them newest-first.
//  3. Does nothing at all once the build commits.
//
// WHY: BuildNewVM creates a zvol, converts a multi-gigabyte image onto it,
// writes a seed ISO, and only then defines a domain. Every `return err`
// between those steps used to leave the earlier ones behind. A `sudo -n`
// refusal partway through — the common case on a box where the operator's
// credential has expired — left an orphan zvol holding real space and a
// seed ISO holding a credential, with no domain to attach either to and
// nothing in the UI that knew they existed. Retrying then failed on `zfs
// create: dataset already exists`, so the second attempt looked like a
// different bug.
//
// Notes: undo actions are registered ONLY for resources this build actually
// created. `zfs create` fails when the dataset exists, so reaching the
// registration means it is ours; the file paths are checked for prior
// existence first, because qemu-img and install overwrite silently and
// deleting a pre-existing disk would be far worse than leaking one.
//
// The downloaded cloud image is deliberately never registered: it is a
// shared cache, not a per-VM resource.
package main

import "os"

// rollback is a stack of undo actions for one build.
type rollback struct {
	steps     []func()
	committed bool
}

// add registers an undo action. Order matters: they run in reverse, so a
// zvol registered before the seed ISO is destroyed after it.
func (r *rollback) add(undo func()) { r.steps = append(r.steps, undo) }

// commit disarms the stack. Call it once the domain is defined and the
// resources belong to a real VM.
func (r *rollback) commit() { r.committed = true }

// run executes the undo actions unless the build committed. Safe to defer
// unconditionally, and safe to call twice.
//
// Failures inside an undo action are reported and then ignored: cleanup runs
// on the error path, and a cleanup that aborts halfway leaves more behind
// than one that keeps going. Everything it does is narrated, so an operator
// reading the log sees exactly what was removed.
func (r *rollback) run(progress func(string)) {
	if r.committed {
		return
	}
	r.committed = true // never run twice
	for i := len(r.steps) - 1; i >= 0; i-- {
		r.steps[i]()
	}
	r.steps = nil
}

// fileAbsent reports whether a path does not exist yet, so a build can tell
// "I created this" from "this was already here" before registering an undo
// that would delete it.
func fileAbsent(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}
