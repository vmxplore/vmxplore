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
	"fmt"
	"os/exec"
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

// cloneFollowUpLabel is the checkbox text for a golden.
func cloneFollowUpLabel(golden string, port int, n int) string {
	switch cloneFollowUp(golden, port) {
	case "wall":
		return "When done, open the VDI wall — every desktop on one page"
	case "rdp":
		return fmt.Sprintf("When done, open an RDP session to each of the %d desktop(s)", n)
	case "browser":
		return fmt.Sprintf("When done, open each of the %d instance(s) in a browser tab", n)
	}
	return "Nothing to open when done — this golden serves no port"
}

// rdpClientArgv is the local RDP client to launch for one address: Remmina
// when present (it prompts for the login, which is the tile's guest
// account), else FreeRDP without a password on the command line — a
// credential in argv is a credential in `ps`. Empty when neither exists.
func rdpClientArgv(ip string) []string {
	if _, err := exec.LookPath("remmina"); err == nil {
		return []string{"remmina", "-c", "rdp://" + ip}
	}
	for _, c := range []string{"xfreerdp3", "xfreerdp"} {
		if _, err := exec.LookPath(c); err == nil {
			return []string{c, "/v:" + ip, "/cert:ignore", "/dynamic-resolution"}
		}
	}
	return nil
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
