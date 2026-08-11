package main

import (
	"strings"
	"testing"
)

// A numeric VM name made meta-data invalid: `instance-id: 11` is the INTEGER
// eleven in YAML, and cloud-init's NoCloud datasource then never initialises
// — so no user, no password, no runcmd, and a VM that boots as a bare image
// looking like the seed was ignored. Found 2026-08-11 on a VM named "11".
func TestNumericNamesStayStringsInMetaData(t *testing.T) {
	for _, name := range []string{"11", "234", "0", "007"} {
		meta := metaData(name)
		if !strings.Contains(meta, `instance-id: "`+name+`"`) {
			t.Errorf("instance-id must be a quoted string for %q:\n%s", name, meta)
		}
		if !strings.Contains(meta, `local-hostname: "`+name+`"`) {
			t.Errorf("local-hostname must be a quoted string for %q:\n%s", name, meta)
		}
	}
}

// And it must parse as YAML with both values as strings, which is the thing
// that actually matters to cloud-init.
func TestMetaDataValuesAreStrings(t *testing.T) {
	meta := metaData("11")
	// A bare 11 would render without quotes; the quotes are the assertion.
	if strings.Contains(meta, "instance-id: 11\n") {
		t.Errorf("numeric instance-id left unquoted:\n%s", meta)
	}
}

func TestOrdinaryNamesUnaffected(t *testing.T) {
	meta := metaData("www")
	if !strings.Contains(meta, `instance-id: "www"`) {
		t.Errorf("alphabetic names should still be quoted, consistently:\n%s", meta)
	}
}
