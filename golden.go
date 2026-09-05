// golden.go — Make Golden: turn a prepared VM into a sealed, clonable
// template.
//
// What it does, in order:
//  1. Gracefully shuts the domain down (and waits — virsh shutdown is
//     async; snapshotting a running root would capture a dirty image).
//  2. Seals the disk so every clone boots as a fresh machine: kldload-seal
//     on the zvol where present (the kldload host tool), else virt-sysprep
//     on the domain (the generic libguestfs route), else a loud warning —
//     an unsealed golden means clones share machine-id and ssh host keys.
//  3. Snapshots the zvol @golden — the anchor planCloneGolden clones from.
//
// Why: build one VM carefully — cloud image or a hand-run installer —
// seal it once, clone out clones for free. This is the klab/kimage golden
// workflow generalized to any ZFS+KVM host, capability-probed as always.
//
// Notes: re-goldening destroys the old @golden first; ZFS refuses while
// clones depend on it, which is exactly the protection an operator wants —
// the error names the clones instead of orphaning them.
package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// MakeGolden runs the shutdown → seal → snapshot flow. progress gets one
// line per step and may be called from any goroutine.
func MakeGolden(r Row, progress func(string)) error {
	if r.Synthetic {
		return fmt.Errorf("no domain behind this row")
	}
	if r.DS == nil {
		return fmt.Errorf("%s has no zvol — golden templates need ZFS underneath", r.D.Name)
	}
	name, ds := r.D.Name, r.DS.Name

	// 1 — a clean shutdown, waited for
	state := func() string {
		out, _ := virshOut("domstate", name)
		return strings.TrimSpace(string(out))
	}
	if state() == "running" {
		if err := runStep(progress, false,
			append(virsh(), "shutdown", name)...); err != nil {
			return err
		}
		progress("waiting for " + name + " to shut down…")
		deadline := time.Now().Add(2 * time.Minute)
		for state() != "shut off" {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not shut down within 2 minutes — "+
					"force it off and retry", name)
			}
			time.Sleep(2 * time.Second)
		}
	}

	// 2 — seal (capability-probed; try each tool, warn loudly rather than fake it)
	//
	// A FAILING tool falls through to the next one, and exhausting them all is
	// a warning, not an error. This used to `return err` on the first failure,
	// which made one tool's bad day fatal to the whole operation: on 2026-08-07
	// `kldload-seal /dev/zvol/rpool/vms/fleet` exited 1, MakeGolden returned,
	// BuildFleet aborted with "make golden: ...", and the run ended having
	// built exactly one VM and zero clones -- reported as "EZ Fleet launches
	// one and dies". virt-sysprep was installed on that host the whole time and
	// was never tried, because the switch had already committed to the first
	// matching case.
	//
	// Continuing unsealed is a REAL cost, not a shrug: every clone inherits this
	// machine's machine-id and ssh host keys. The default branch below has
	// always accepted that trade when no tool exists at all, so accepting it
	// when the tools fail is the consistent choice -- and a fleet of clones that
	// need `systemd-firstboot` is recoverable, where no fleet at all is not.
	sealed := false
	if havePath("kldload-seal") {
		if err := runStep(progress, true, "kldload-seal", "/dev/zvol/"+ds); err != nil {
			progress(fmt.Sprintf("kldload-seal failed (%v) — trying virt-sysprep", err))
		} else {
			sealed = true
		}
	}
	if !sealed && havePath("virt-sysprep") {
		if err := runStep(progress, true, "virt-sysprep", "-d", name); err != nil {
			progress(fmt.Sprintf("virt-sysprep failed (%v)", err))
		} else {
			sealed = true
		}
	}
	if !sealed {
		progress("WARNING: could not seal this golden — it keeps this machine's " +
			"identity, so every clone will share its machine-id and ssh host keys. " +
			"Run `systemd-firstboot --setup-machine-id` and regenerate host keys in " +
			"each clone, or re-golden once a seal tool works.")
	}

	// 3 — the @golden anchor (re-golden replaces the old one; ZFS refuses
	// while clones depend on it, naming them — the right failure)
	if exec.Command(zfsArgv("list", ds+"@golden")[0],
		zfsArgv("list", ds+"@golden")[1:]...).Run() == nil {
		if err := runStep(progress, true, zfsArgv("destroy", ds+"@golden")...); err != nil {
			return fmt.Errorf("%v — existing clones depend on the old golden; "+
				"delete or promote them first", err)
		}
	}
	if err := runStep(progress, true, zfsArgv("snapshot", ds+"@golden")...); err != nil {
		return err
	}
	// An appliance's second disk is its in-guest pool: the database, the
	// media, the thing the tile exists for. A golden that sealed only the
	// root would clone out clones that boot and then have nothing to serve.
	// The disk list comes from the domain, the same list planCloneFrom
	// walks, so what gets a @golden here is exactly what a clone looks for.
	for _, extra := range domainZvols(r)[1:] {
		if !datasetExists(extra) {
			return fmt.Errorf("%s: disk %s is in the domain but not on the pool"+
				" — recreate it or detach it before making a golden", name, extra)
		}
		if datasetExists(extra + "@golden") {
			if err := runStep(progress, true, zfsArgv("destroy", extra+"@golden")...); err != nil {
				return fmt.Errorf("%v — existing clones depend on the old data golden", err)
			}
		}
		if err := runStep(progress, true, zfsArgv("snapshot", extra+"@golden")...); err != nil {
			return err
		}
		progress(extra + " is golden too — clones get their pool")
	}
	progress(name + " is golden — right-click → Clone clones out instant copies")
	return nil
}

func havePath(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

// planCloneGolden clones FROM the @golden anchor: no new snapshot, the
// clone shares the sealed template's blocks. The GUI picks this over
// planClone whenever @golden exists on the source.
func planCloneGolden(r Row, newName string) (verbPlan, error) {
	// One builder for both shapes: this used to rewrite planClone's command
	// list by index ("the root pair is cmds[0:2], the data pair is next")
	// and every disk-list change threatened to shift those indexes under
	// it. planCloneFrom walks the domain's disks once and picks the anchor
	// per disk instead.
	return planCloneFrom(r, newName, true)
}

// ─── Naming the things we clone out ──────────────────────────────────
//
// Naming N clones is a chore that stops nobody from cloning but slows
// everybody down, and the names rarely mean anything a week later. Leaving
// the name blank generates one per clone instead.

// cloneNameAlphabet omits the glyphs that read ambiguously when someone is
// copying a name off a console: 0/O, 1/l/I. A name exists to be retyped
// correctly at 3am, and 31 symbols over 10 places is still ~10^15 of room.
const cloneNameAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// randomName returns prefix + 10 random symbols, e.g. clone-h2g1j2hbqr.
//
// Returns: the name, or an error if the system entropy source failed —
// which is not survivable and must not be papered over with a timestamp.
//
// NOTE: the modulo below is very slightly biased toward the front of the
// alphabet. That matters for keys and does not matter here: this is a label
// whose only job is to be unique among a few dozen VMs, and uniqueness is
// enforced by freshCloneName checking the host, not by the distribution.
func randomName(prefix string) (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("no entropy for a generated name: %w", err)
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = cloneNameAlphabet[int(v)%len(cloneNameAlphabet)]
	}
	return prefix + string(out), nil
}

// nameInUse reports whether anything on the target host already answers to
// name — either a libvirt domain or a dataset under zfsParent.
//
// WHY BOTH: they fail differently and both failures are ugly. A surviving
// domain makes virt-clone refuse; a leftover DATASET with no domain (the
// residue of a half-undefined VM) makes `zfs clone` refuse halfway through
// a batch, after some clones already exist. Checking only libvirt is how a
// generated name still collides.
//
// A probe that cannot run is treated as "in use": refusing a name costs one
// retry, while wrongly declaring it free costs a failed batch.
func nameInUse(name, zfsParent string) bool {
	di := virsh("dominfo", name)
	if exec.Command(di[0], di[1:]...).Run() == nil {
		return true
	}
	if zfsParent != "" {
		chk := zfsArgv("list", zfsParent+"/"+name)
		if exec.Command(chk[0], chk[1:]...).Run() == nil {
			return true
		}
	}
	return false
}

// freshName returns a generated name nothing is using yet.
//
// Args:    prefix     what the name starts with ("clone-", "golden-");
//
//	         taken      names already handed out in THIS batch (mutated —
//
//		           a name is reserved the moment it is returned, since
//		           the clone it belongs to does not exist yet and so
//		           cannot be found by nameInUse);
//		zfsParent  the dataset the clones will live under.
//
// Returns: the name, or an error after 64 failed attempts.
//
// The retry bound exists so a broken probe (a wedged virsh, a pool that
// answers "yes" to everything) fails loudly instead of spinning forever
// inside a click handler.
func freshName(prefix string, taken map[string]bool, zfsParent string) (string, error) {
	for i := 0; i < 64; i++ {
		n, err := randomName(prefix)
		if err != nil {
			return "", err
		}
		if taken[n] || nameInUse(n, zfsParent) {
			continue
		}
		taken[n] = true
		return n, nil
	}
	return "", errors.New("could not generate an unused name in 64 tries — " +
		"is virsh or zfs answering yes to everything?")
}
