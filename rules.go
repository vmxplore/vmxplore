// rules.go — estate grouping & snapshot classification, driven by a rules file.
//
// The load-bearing design rule (docs/VM-CONSOLE-DESIGN.md): grouping and
// classification are DATA, not Go. The compiled core must not know any site's
// naming convention — kldload's lives in rules/kldload.rules, embedded as one
// profile among several and overridable at /etc/vmxplore/rules or --rules.
//
// Inputs:  a rules file (see rules/kldload.rules for the format).
// Outputs: Ruleset.GroupFor(domain) → estate group label ("" = ungrouped),
//
//	Ruleset.ClassifySnap(name) → class string,
//	Ruleset.SnapSummary(snaps) → per-class counts.
//
// Notes: first match wins in both tables, so order in the file matters —
// goldens before cluster patterns. A snapshot matching no rule is class
// "human": on every convention observed, automation always prefixes, so an
// unprefixed name is an operator's.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

//go:embed rules/kldload.rules
var kldloadRules string

//go:embed rules/generic.rules
var genericRules string

// SnapHuman is the class of operator-made snapshots — the ✎ count in the
// estate table — and the default for names no rule matches.
const SnapHuman = "human"

// SnapNoise is the class collapsed to a bare count everywhere in the UI.
const SnapNoise = "noise"

type snapRule struct {
	class  string
	prefix string
}

type groupRule struct {
	re    *regexp.Regexp
	label string // may contain $1…$9 capture expansions
}

// Ruleset is a parsed rules file. Zero value = everything human, ungrouped.
type Ruleset struct {
	snaps  []snapRule
	groups []groupRule
	Source string // where it came from, for --version/status display
}

// ParseRules parses the rules-file format. Unknown directives and syntax
// errors fail loudly — a silently half-loaded ruleset misclassifies 16k
// snapshots without anyone noticing.
func ParseRules(src, origin string) (*Ruleset, error) {
	rs := &Ruleset{Source: origin}
	for ln, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		switch {
		case f[0] == "snap" && len(f) == 3:
			rs.snaps = append(rs.snaps, snapRule{class: f[1], prefix: f[2]})
		case f[0] == "group" && len(f) >= 3:
			re, err := regexp.Compile(f[1])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: bad regex %q: %w", origin, ln+1, f[1], err)
			}
			rs.groups = append(rs.groups, groupRule{re: re, label: strings.Join(f[2:], " ")})
		default:
			return nil, fmt.Errorf("%s:%d: unrecognized rule %q", origin, ln+1, line)
		}
	}
	return rs, nil
}

// LoadRules picks the ruleset: explicit path → /etc/vmxplore/rules →
// embedded kldload profile (on a kldload host) → embedded generic profile.
// Only the explicit path is fatal on error — it was asked for by name.
func LoadRules(path string) (*Ruleset, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return ParseRules(string(b), path)
	}
	if b, err := os.ReadFile("/etc/vmxplore/rules"); err == nil {
		return ParseRules(string(b), "/etc/vmxplore/rules")
	}
	if IsKldload() {
		return ParseRules(kldloadRules, "builtin:kldload")
	}
	return ParseRules(genericRules, "builtin:generic")
}

// ClassifySnap classifies a snapshot by name (the part after '@').
func (rs *Ruleset) ClassifySnap(name string) string {
	for _, r := range rs.snaps {
		if strings.HasPrefix(name, r.prefix) {
			return r.class
		}
	}
	return SnapHuman
}

// SnapSummary counts snapshots per class. Callers render class "noise" as a
// bare count and class "human" as the ✎ figure.
func (rs *Ruleset) SnapSummary(snaps []string) map[string]int {
	m := make(map[string]int, 4)
	for _, s := range snaps {
		m[rs.ClassifySnap(s)]++
	}
	return m
}

// GroupFor returns the estate group label for a domain name, expanding
// $1…$9 from the matching rule's capture groups. "" means ungrouped.
func (rs *Ruleset) GroupFor(domain string) string {
	for _, g := range rs.groups {
		m := g.re.FindStringSubmatchIndex(domain)
		if m == nil {
			continue
		}
		return string(g.re.ExpandString(nil, g.label, domain, m))
	}
	return ""
}

// ─── kldload detection ──────────────────────────────────────────────────────
// Copied shape from zxplore (zfs.go): detection gates cosmetic flair and the
// default ruleset ONLY — never whether a capability exists. Capabilities gate
// on LookPath/probes (HasZFS, kldload-db present, …), per the design rule
// that build-iso.sh once violated with profile-name gating.

// IsKldload reports whether this is a kldload host, so vmx can pick the
// kldload ruleset and light up k-command delegation. Fully generic otherwise.
func IsKldload() bool {
	for _, p := range []string{"/usr/local/bin/kbe", "/etc/kldload"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// vmKTools are the kldload VM tools vmx surfaces delegation flair for when it
// detects them. vmx stays fully generic without them.
// toolGroup is one section of the kldload launcher. The tab carries 27
// tools; ungrouped they were a wall you had to read end to end to find
// anything. Sections give the eye somewhere to stop.
//
// Order is by how often a section is reached for, not alphabetically:
// machines first, then the things you do to them, then the estate, then
// the things you open when something is wrong.
type toolGroup struct {
	Name  string
	Tools []string
}

var vmKToolGroups = []toolGroup{
	{"Virtual machines", []string{
		"kvm-create", "kvm-clone", "kspawn", "kvm-win", "kvm-list", "kvm-delete",
	}},
	{"Images & snapshots", []string{
		"kimage", "kvm-snap", "ksnap", "kexport", "kbe", "kldload-snapshot",
	}},
	// The storage and network consoles. All three ZFS surfaces were once
	// missing from this tab entirely, so a kldload host showed every way to
	// make a VM and no way to reach its storage. wgx joins them because a
	// kldload host runs the WireGuard estate too and the operator should not
	// have to remember a second launcher for it.
	{"Storage & network", []string{
		"zxplore", "kzfs-lab", "kst", "wgx",
	}},
	{"Cluster", []string{
		"klab", "kube-cluster", "kube-init",
	}},
	// Reachable FROM the estate is the point — you want these at the moment
	// a machine looks wrong, not in another window (2026-08-09 operator
	// call). Recovery sits with them: it is the same moment, later.
	{"Health & recovery", []string{
		"kst-dashboard", "kldload-sysdiag", "kldload-doctor",
		"kldload-console", "krecovery",
	}},
	{"Demos & assistant", []string{
		"kvm-demo", "kube-demo", "bob",
	}},
}

// vmKTools is the flat list, derived from the groups so the two can never
// disagree about which tools exist.
var vmKTools = func() []string {
	var out []string
	for _, g := range vmKToolGroups {
		out = append(out, g.Tools...)
	}
	return out
}()

// KldloadToolGroups returns the sections with only the tools actually
// installed, dropping any section left empty. A host without a cluster
// should not read a "Cluster" heading over nothing.
func KldloadToolGroups() []toolGroup {
	if !IsKldload() {
		return nil
	}
	var out []toolGroup
	for _, g := range vmKToolGroups {
		var present []string
		for _, t := range g.Tools {
			if _, err := exec.LookPath(t); err == nil {
				present = append(present, t)
			}
		}
		if len(present) > 0 {
			out = append(out, toolGroup{g.Name, present})
		}
	}
	return out
}

// KldloadTools returns the kldload VM tools present on this host (nil on a
// generic system).
func KldloadTools() []string {
	if !IsKldload() {
		return nil
	}
	var found []string
	for _, t := range vmKTools {
		if _, err := exec.LookPath(t); err == nil {
			found = append(found, t)
		}
	}
	return found
}
