//go:build gui

// fleet.go — the one-click fleet: build a golden image, then stamp N clones
// off it. The "EZ build/clone" button.
//
// What it does, in order:
//  1. BuildNewVM to create the source (cloud image + optional post-install,
//     or an installer ISO the operator has already provisioned).
//  2. Waits for cloud-init to settle, then MakeGolden (seal + @golden).
//  3. Stamps count clones off @golden via planCloneGolden + runPlan,
//     each an instant zero-copy ZFS clone.
//
// Why: this is the whole value proposition in one gesture — "give me five
// Fedora boxes." The primitives already exist and are each tested; this is
// the orchestration that turns them into a fleet. Cloud mode only: a golden
// must be a finished system, which an unattended cloud image is and a
// half-run ISO installer is not.
//
// Notes: cloud-init has no host-visible "done" without a guest-agent
// round-trip, so the settle watches the DISK instead — a first boot that is
// still working grows the zvol, and one that has finished stops. That
// matters because the pause used to be a fixed 90 seconds, tuned for a base
// image plus a short post-install. A desktop takes five to ten minutes, so
// the golden would have been sealed mid-install and every clone stamped
// from a half-built system.
package main

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// BuildFleet runs source → golden → N clones. progress streams one line per
// step. Clone names are <base>-1..<base>-N. Returns the first error; clones
// already made stay made (each is independent).
func BuildFleet(spec NewVMSpec, count int, zfsParent string, progress func(string)) error {
	if spec.install() {
		return fmt.Errorf("fleet needs a cloud image (a golden must be a finished system)")
	}
	if count < 1 || count > 64 {
		return fmt.Errorf("clone count must be 1–64")
	}
	if zfsParent == "" {
		return fmt.Errorf("fleet needs a ZFS home for VMs (clones are zvol clones)")
	}

	progress(fmt.Sprintf("[1/3] building golden source %q…", spec.Name))
	if err := BuildNewVM(spec, zfsParent, progress); err != nil {
		return fmt.Errorf("build source: %w", err)
	}

	// Wait for first boot to actually finish, rather than for 90 seconds.
	if err := waitFirstBoot(zfsParent+"/"+spec.Name, spec.Desktop != "" &&
		spec.Desktop != "none", progress); err != nil {
		return err
	}
	src := Row{
		D:  Dom{Name: spec.Name, State: "running"},
		DS: &Dataset{Name: zfsParent + "/" + spec.Name},
	}
	if err := MakeGolden(src, progress); err != nil {
		return fmt.Errorf("make golden: %w", err)
	}

	progress(fmt.Sprintf("[3/3] stamping %d clones off the golden…", count))
	started := 0
	for i := 1; i <= count; i++ {
		cn := fmt.Sprintf("%s-%d", spec.Name, i)
		plan, err := planCloneGolden(src, cn)
		if err != nil {
			return fmt.Errorf("plan clone %s: %w", cn, err)
		}
		progress(fmt.Sprintf("  clone %d/%d → %s", i, count, cn))
		if err := runPlan(plan); err != nil {
			return fmt.Errorf("clone %s: %w", cn, err)
		}
		// The golden is shut off, so every clone off it is defined and dark.
		// A "fleet ready" line over N machines that are all powered down is
		// the same report a total failure would produce, and it read as one
		// (operator, 2026-08-15). Booting is part of delivering the fleet.
		//
		// A clone that will not start is reported and the run continues: the
		// machine exists and can be started by hand, and aborting here would
		// leave the remaining clones unstamped over one bad boot.
		sp, serr := planStart(Row{D: Dom{Name: cn, State: "shut off"}})
		if serr == nil {
			serr = runPlan(sp)
		}
		if serr != nil {
			progress(fmt.Sprintf("  WARNING: %s was created but would not start: %v", cn, serr))
		} else {
			started++
		}
	}
	progress(fmt.Sprintf("fleet ready: %s (golden) + %d clones, %d running",
		spec.Name, count, started))
	return nil
}

// waitFirstBoot blocks until the guest's first boot stops changing its disk.
//
// WHY the disk: cloud-init reports "done" only inside the guest, and reaching
// in needs either a guest agent or credentials and an ssh round-trip. The
// zvol is visible from here and tells the same story — a first boot that is
// installing grows it, and one that has finished stops. Sealing a golden is
// the one moment where being early is unrecoverable: every clone inherits
// whatever state the disk was in.
//
// Args:    ds        the golden's dataset
//
//	desktop   true when a desktop was requested, which changes the
//	          budget from "a base image settles" to "1.5-3GB installs"
//	progress  the pipeline's narrator
//
// Returns: nil once the disk has been quiet for quietFor, or when the cap is
// reached — the cap returns nil, not an error, because a slow guest is not a
// failed one and refusing to seal would strand the fleet entirely.
// Failure modes callers must handle: `zfs list` failing outright, which is
// reported, because that means the dataset is not there at all.
func waitFirstBoot(ds string, desktop bool, progress func(string)) error {
	const (
		poll     = 15 * time.Second
		quietFor = 3 // consecutive quiet samples
		// A rewritten block or a log line moves `used` by a little even on
		// an idle guest; only growth above this counts as work.
		growthMB = 8
	)
	cap := 4 * time.Minute
	what := "first boot"
	if desktop {
		// A desktop is 1.5-3GB over a network that may be slow.
		cap = 25 * time.Minute
		what = "desktop install"
	}
	progress(fmt.Sprintf("[2/3] waiting for %s to finish (watching the disk)…", what))

	deadline := time.Now().Add(cap)
	var last int64
	quiet := 0
	// A floor, so a guest that has not started writing yet is not mistaken
	// for one that has finished.
	time.Sleep(45 * time.Second)
	for time.Now().Before(deadline) {
		used, err := zvolUsed(ds)
		if err != nil {
			return fmt.Errorf("watching %s: %w", ds, err)
		}
		if grew := used - last; grew > int64(growthMB)<<20 {
			quiet = 0
			progress(fmt.Sprintf("  still working — %d MB written", used>>20))
		} else {
			quiet++
			if quiet >= quietFor {
				progress(fmt.Sprintf("  settled at %d MB; sealing golden…", used>>20))
				return nil
			}
		}
		last = used
		time.Sleep(poll)
	}
	progress(fmt.Sprintf("  %s still busy after %s — sealing anyway", what, cap))
	return nil
}

// zvolUsed returns a dataset's used bytes.
//
// -Hp: no headers, and PARSABLE — plain bytes rather than "1.03G", which is
// unparseable and, worse, rounds away exactly the small deltas this watch
// depends on.
func zvolUsed(ds string) (int64, error) {
	out, err := exec.Command(zfsArgv("list", "-Hpo", "used", ds)[0],
		zfsArgv("list", "-Hpo", "used", ds)[1:]...).Output()
	if err != nil {
		return 0, err
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		return 0, fmt.Errorf("unparsable used value %q: %w", out, err)
	}
	return n, nil
}
