// reconcile_cmd.go — `vmx --reconcile`: clear the unreconciled group.
//
// What it does, in order:
//  1. Builds the estate exactly as --once does (live libvirt + live ZFS +
//     the register annotations).
//  2. Splits the unreconciled group into REGISTER ghosts — a register row for
//     a VM libvirt does not have, nothing on disk — and ORPHAN ZVOLS, which
//     are a volume with data in it.
//  3. Deregisters every ghost. Lists the orphans and leaves them alone unless
//     --orphans was passed.
//  4. Re-reads the estate and reports what is left, because a count of
//     commands issued is not a count of rows cleared.
//
// WHY this exists: onyx accumulated 21 of these — imgtest-*, dmcheck-*,
// detest-*, all from test harnesses that deleted a VM from libvirt and never
// told state.db. Clearing them by hand was 21 keystrokes in the TUI, which is
// how an estate view stops being read at all (operator, 2026-09-20: "there
// needs to be a command to clean them").
//
// WHY the split is not a prompt: a register ghost costs nothing to forget and
// cannot lose data, so it goes without asking. An orphan zvol may be the only
// copy of something, and `zfs destroy -r` takes its snapshots with it — so
// that one needs the operator to have said so in the argv. A flag is an
// interface; a confirmation box is a nag. This tool does not nag.
//
// Exit: 0 when nothing is left unreconciled (or nothing needed doing), 1 when
// a deregistration failed, 2 when orphans remain and --orphans was not given.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// unreconciledSplit sorts the unreconciled rows by what it costs to clear
// them. A row with a dataset behind it holds data; a row without one is a
// register entry and nothing more.
func unreconciledSplit(groups []GroupRows) (ghosts, orphans []Row) {
	for _, g := range groups {
		if g.Label != groupUnreconciled {
			continue
		}
		for _, r := range g.Rows {
			if r.DS != nil {
				orphans = append(orphans, r)
			} else {
				ghosts = append(ghosts, r)
			}
		}
	}
	return ghosts, orphans
}

// runReconcile is the --reconcile entry point.
func runReconcile(rs *Ruleset, destroyOrphans bool) int {
	lv, err := ConnectSystem()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: libvirt: %v\n", err)
		return 1
	}
	defer lv.Close()

	groups, err := reconcileEstate(lv, rs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 1
	}
	ghosts, orphans := unreconciledSplit(groups)

	if len(ghosts) == 0 && len(orphans) == 0 {
		fmt.Println("nothing unreconciled")
		return 0
	}

	failed := 0
	for _, r := range ghosts {
		fmt.Printf("forget  %-32s %s\n", r.D.Name, strings.Join(r.Notes, "; "))
		for _, argv := range dbUnregisterVM(r.D.Name) {
			if len(argv) == 0 {
				continue
			}
			// Each command's own output is the record of what happened; only
			// a failure is worth interrupting the list for.
			if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "  FAILED: %s: %v: %s\n",
					strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
			}
		}
	}

	for _, r := range orphans {
		if !destroyOrphans {
			fmt.Printf("orphan  %-32s %s  (kept — --orphans destroys it)\n",
				r.D.Name, r.Backing)
			continue
		}
		fmt.Printf("destroy %-32s %s\n", r.D.Name, r.Backing)
		plan, perr := planReconcile(r)
		if perr != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", perr)
			continue
		}
		if rerr := runPlan(plan); rerr != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", rerr)
		}
	}

	// Outcome, not exit code: re-read the estate rather than trusting that
	// every command that returned 0 actually cleared a row. kldload-db
	// vm-delete is a soft delete — an UPDATE whose WHERE clause matching
	// nothing is a perfectly successful no-op.
	after, err := reconcileEstate(lv, rs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: could not re-read the estate to verify: %v\n", err)
		return 1
	}
	ga, oa := unreconciledSplit(after)
	fmt.Printf("\nunreconciled: %d -> %d  (%d register ghost(s), %d orphan zvol(s) left)\n",
		len(ghosts)+len(orphans), len(ga)+len(oa), len(ga), len(oa))

	switch {
	case failed > 0:
		return 1
	case len(ga) > 0:
		// Rows that survived their own deregistration are a defect, not a
		// tidy-up that did not apply.
		fmt.Fprintf(os.Stderr,
			"vmx: %d register ghost(s) survived deregistration — kldload-db may not be writable here\n", len(ga))
		return 1
	case len(oa) > 0:
		return 2
	}
	return 0
}

// reconcileEstate is one estate read: the same joins BuildEstate wants, minus
// the CPU sampling that only a live view needs.
func reconcileEstate(lv *LV, rs *Ruleset) ([]GroupRows, error) {
	doms, err := lv.Estate()
	if err != nil {
		return nil, err
	}
	var dss map[string]*Dataset
	var snaps map[string][]string
	if HasZFS() {
		dss, _ = ListDatasets()
		snaps, _ = ListSnapshots()
	}
	return BuildEstate(doms, dss, snaps, rs, LoadAnnotations()), nil
}
