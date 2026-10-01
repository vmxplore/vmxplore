//go:build gui

// firecracker_gui.go — the GUI-only half of firecracker.go: the goldens
// list the Clone dialog and the branch rows read, and the streaming runner
// the batch window feeds. Split off because `staticcheck ./...` without the
// gui tag reports anything only gui.go reaches as unused (make check,
// 2026-09-05).
package main

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"time"
)

// FCGolden is one row of `kfire goldens --json`.
var (
	fcGoldenC  []FCGolden
	fcGoldenAt time.Time // the fcAt the goldens were read at
)

// fcGoldensCached is the goldens on the instances' clock: read again
// whenever the instances were, so one fcInvalidate refreshes both.
func fcGoldensCached() []FCGolden {
	if !fcHaveKfire() {
		return nil
	}
	fcSlowMu.Lock()
	defer fcSlowMu.Unlock()
	fcRefresh()
	fcMu.Lock()
	at, stale := fcAt, !fcGoldenAt.Equal(fcAt)
	fcMu.Unlock()
	if stale {
		gs, err := fcGoldensRead() // 4-6 s: no data lock held
		if err != nil {
			auditLog("kfire goldens --json: "+err.Error(), 1)
			gs = nil
		}
		fcMu.Lock()
		fcGoldenC, fcGoldenAt = gs, at
		fcMu.Unlock()
	}
	fcMu.Lock()
	defer fcMu.Unlock()
	return fcGoldenC
}

// fcGoldensSnapshot is what the last refresh read, and never runs kfire: for
// the tree's painters and anything else on the UI thread, where a 4-6 s
// `kfire goldens` froze the window (onyx, 2026-09-28). The background tick
// keeps it fresh through fcGoldensCached.
func fcGoldensSnapshot() []FCGolden {
	fcMu.Lock()
	defer fcMu.Unlock()
	return fcGoldenC
}

// streamCmd runs argv and hands every output line to log as it arrives,
// which is what a clone needs: each instance prints as it comes up, and a
// ten-instance --wait is half a minute nobody wants to stare at a blank
// window for. ctx cancels by killing the process. Audited like a plan.
func streamCmd(ctx context.Context, log func(string), argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			log(sc.Text())
		}
		close(done)
	}()
	err := cmd.Wait()
	_ = pw.Close()
	<-done
	rc := 0
	if err != nil {
		rc = 1
	}
	auditLog(strings.Join(argv, " "), rc)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
