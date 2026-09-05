// rules_test.go — the embedded rulesets must classify and group the real
// estate's naming conventions; these names are copied from a live host.
package main

import "testing"

func kldloadRS(t *testing.T) *Ruleset {
	t.Helper()
	rs, err := ParseRules(kldloadRules, "test:kldload")
	if err != nil {
		t.Fatalf("embedded kldload rules do not parse: %v", err)
	}
	return rs
}

func TestClassifySnap(t *testing.T) {
	rs := kldloadRS(t)
	cases := map[string]string{
		"autosnap_2026-08-07_15:00:02_frequently": "noise",
		"auto-20260807-150000":                    "noise",
		"manual-before-upgrade":                   "human",
		"golden":                                  "golden",
		"clone-20260705_143916906897718":          "provenance",
		"create-20260705":                         "provenance",
		"pre-kube-init-20260705":                  "checkpoint",
		"repl-fiend-20260801":                     "repl",
		"my-adhoc-snap":                           "human", // unmatched → human
	}
	for name, want := range cases {
		if got := rs.ClassifySnap(name); got != want {
			t.Errorf("ClassifySnap(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestGroupFor(t *testing.T) {
	rs := kldloadRS(t)
	cases := map[string]string{
		"k8s-golden":         "goldens",
		"klab-golden-fedora": "goldens",
		// The desktop golden lineage carries its kind, not the word
		// "golden", so it needs its own rule to reach the group.
		"klab-desktop-debian":     "goldens",
		"klab-desktop-fedora":     "goldens",
		"klab-desktop-ubuntu2404": "goldens",
		"win-10-golden":           "goldens",
		"klab-blue-fedora":        "klab",
		"klab-green-ubuntu":       "klab",
		// the ztest GOLDEN is a golden; a numbered clone stays a lab machine
		"klab-ztest-debian":   "goldens",
		"klab-ztest-debian-1": "klab",
		"kspawn-demo-01":      "kspawn: demo",
		"kldload-cp":          "k8s: kldload",
		"kldload-w-2":         "k8s: kldload", // dashed worker form on the live estate
		"demo-leftover":       "",             // ungrouped
	}
	for name, want := range cases {
		if got := rs.GroupFor(name); got != want {
			t.Errorf("GroupFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSnapSummary(t *testing.T) {
	rs := kldloadRS(t)
	sum := rs.SnapSummary([]string{
		"autosnap_a", "autosnap_b", "manual-x", "golden", "clone-1",
	})
	if sum["noise"] != 2 || sum["human"] != 1 || sum["golden"] != 1 ||
		sum["provenance"] != 1 {
		t.Errorf("SnapSummary wrong: %v", sum)
	}
}

func TestParseRulesRejectsGarbage(t *testing.T) {
	if _, err := ParseRules("frobnicate all the things", "t"); err == nil {
		t.Error("garbage directive parsed without error")
	}
	if _, err := ParseRules("group ((( broken", "t"); err == nil {
		t.Error("bad regex parsed without error")
	}
}

func TestGenericRulesParse(t *testing.T) {
	rs, err := ParseRules(genericRules, "test:generic")
	if err != nil {
		t.Fatalf("embedded generic rules do not parse: %v", err)
	}
	if got := rs.ClassifySnap("zfs-auto-snap_hourly-2026-08-07"); got != "noise" {
		t.Errorf("generic noise classification broken: %q", got)
	}
	if got := rs.GroupFor("anything"); got != "" {
		t.Errorf("generic ruleset should not group, got %q", got)
	}
}

// TestClonesGetTheirOwnSection exercises the real BuildEstate path: a
// machine no rule claims, but which has an origin, is grouped as a clone.
// It must not steal machines an explicit rule already grouped.
func TestClonesGetTheirOwnSection(t *testing.T) {
	rs := kldloadRS(t)
	cases := []struct{ name, origin, want string }{
		// generated and hand-named clones, both off a golden
		{"clone-kesuu7beja", "rpool/vms/klab-desktop-debian@golden", "clones"},
		{"my-test-box", "rpool/vms/klab-golden-fedora@golden", "clones"},
		// no origin: not a clone, so it stays in the ungrouped pile
		{"demo-leftover", "", "ungrouped"},
		{"app-web-stack", "", "apps"},
		{"app-vdi-deskto", "", "apps"},
		{"st-web-stack", "", "apps (self-test)"},
		// an explicit rule wins over lineage: k8s nodes are stamped off
		// k8s-golden and belong to their cluster, not to "clones"
		{"kldload-cp", "rpool/vms/k8s-golden@clone-1", "k8s: kldload"},
		{"kldload-w-2", "rpool/vms/k8s-golden@clone-2", "k8s: kldload"},
		// a golden that is itself a clone still reads as a golden
		{"klab-desktop-fedora", "rpool/vms/seed@golden", "goldens"},
	}

	doms := make([]Dom, 0, len(cases))
	dss := map[string]*Dataset{}
	for _, c := range cases {
		ds := "rpool/vms/" + c.name
		doms = append(doms, Dom{
			Name:  c.name,
			State: "shut off",
			Disks: []Disk{{Target: "vda", Dev: "/dev/zvol/" + ds}},
		})
		dss[ds] = &Dataset{Name: ds, Origin: c.origin, Type: "volume"}
	}

	got := map[string]string{}
	for _, g := range BuildEstate(doms, dss, nil, rs, nil) {
		for _, r := range g.Rows {
			got[r.D.Name] = g.Label
		}
	}
	for _, c := range cases {
		if got[c.name] != c.want {
			t.Errorf("%s (origin %q) grouped as %q, want %q",
				c.name, c.origin, got[c.name], c.want)
		}
	}
}
