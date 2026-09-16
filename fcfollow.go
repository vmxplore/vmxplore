// fcfollow.go — what a clone batch opens when it is done.
//
// The gesture is "clone N, then look at them", and what "look at them"
// means depends on what was cloned: a wall for streamed desktops, one RDP
// session per seat for xrdp desktops, one browser tab per instance for
// anything that serves a page. The clone dialog offers it as a checkbox
// whose label names the follow-up, so the operator knows what is about to
// pop up before pressing Clone ("make the launching browser part a
// question in the clone menu", operator, 2026-09-05).
package main

import (
	"os"
	"path/filepath"
	"strings"
)

// cloneFollowUp classifies a golden by name and port: "wall", "rdp",
// "browser", or "" when there is nothing sensible to open (no port).
func cloneFollowUp(golden string, port int) string {
	switch {
	case strings.HasPrefix(golden, "app-vdi"):
		return "wall"
	case strings.HasPrefix(golden, "app-rdp"), port == 3389:
		return "rdp"
	case port > 0:
		return "browser"
	}
	return ""
}

// newInstancesOf returns the instances of golden that are in after and
// were not in before, with an address — the clones a batch just made.
func newInstancesOf(golden string, before map[string]bool, after []Row) []Row {
	var out []Row
	for _, r := range after {
		if r.FC == nil || r.FC.Golden != golden || before[r.D.Name] {
			continue
		}
		if firstIPv4(r.D.IPs) == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// fcInstanceNames is the set of instance names now, for newInstancesOf.
func fcInstanceNames(rows []Row) map[string]bool {
	m := map[string]bool{}
	for _, r := range rows {
		if r.FC != nil {
			m[r.D.Name] = true
		}
	}
	return m
}

// remminaOneWindowPerSeat makes Remmina open each connection in its own
// window. Remmina is one process and, with its default tab_mode=0, every
// `remmina -c` lands as a tab in the one window — three seats became three
// tabs ("doesn't seem to allow me to use more than 1 window", operator,
// 2026-09-05). tab_mode=3 is "no tabs". The file is Remmina's own
// remmina.pref; the key is rewritten in place or added, and a missing file
// is created with just that key. Returns whether anything changed — a
// running Remmina keeps its old setting in memory, so the caller says so.
func remminaOneWindowPerSeat(prefPath string) (changed bool, err error) {
	b, rerr := os.ReadFile(prefPath)
	if rerr != nil && !os.IsNotExist(rerr) {
		return false, rerr
	}
	lines := strings.Split(string(b), "\n")
	seenSection, seenKey := false, false
	for i, l := range lines {
		switch {
		case strings.TrimSpace(l) == "[remmina_pref]":
			seenSection = true
		case strings.HasPrefix(l, "tab_mode="):
			seenKey = true
			if l != "tab_mode=3" {
				lines[i] = "tab_mode=3"
				changed = true
			}
		}
	}
	if !seenKey {
		if !seenSection {
			lines = append([]string{"[remmina_pref]"}, lines...)
		}
		// after the section header, so the key belongs to it
		for i, l := range lines {
			if strings.TrimSpace(l) == "[remmina_pref]" {
				lines = append(lines[:i+1], append([]string{"tab_mode=3"}, lines[i+1:]...)...)
				break
			}
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(prefPath), 0o700); err != nil {
		return false, err
	}
	return true, os.WriteFile(prefPath, []byte(strings.Join(lines, "\n")), 0o600)
}
