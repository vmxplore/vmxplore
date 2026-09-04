// build_all_test.go — the arithmetic behind parallel build-all, and the
// osinfo probe that decides whether virt-install is even asked for a
// variant. Both pure enough to run anywhere; the probe skips without
// osinfo-query.
package main

import (
	"os/exec"
	"testing"
)

func TestBuildJobs(t *testing.T) {
	cases := []struct {
		name              string
		n, maxTile, avail int
		env               string
		want              int
	}{
		{"onyx today: 10 tiles, 2G max, 21G free", 10, 2048, 21504, "", 8},
		{"everything fits", 10, 2048, 32768, "", 10},
		{"tight host serialises", 10, 2048, 5000, "", 1},
		{"no memory reading at all", 10, 2048, 0, "", 1},
		{"env override wins", 10, 2048, 5000, "4", 4},
		{"env override capped at n", 3, 2048, 5000, "9", 3},
		{"garbage env ignored", 10, 2048, 21504, "lots", 8},
		{"zero env ignored", 10, 2048, 21504, "0", 8},
		{"nothing to build", 0, 2048, 21504, "", 1},
	}
	for _, c := range cases {
		if got := buildJobs(c.n, c.maxTile, c.avail, c.env); got != c.want {
			t.Errorf("%s: buildJobs(%d,%d,%d,%q) = %d, want %d",
				c.name, c.n, c.maxTile, c.avail, c.env, got, c.want)
		}
	}
}

func TestOsinfoKnows(t *testing.T) {
	if _, err := exec.LookPath("osinfo-query"); err != nil {
		t.Skip("osinfo-query not installed")
	}
	// Every osinfo-db has generic linux; no osinfo-db has this string.
	if !osinfoKnows("linux2022") && !osinfoKnows("linux2020") {
		t.Error("a generic linux variant must be known")
	}
	if osinfoKnows("no-such-os-variant-xyz") {
		t.Error("an unknown variant must not be reported as known")
	}
}
