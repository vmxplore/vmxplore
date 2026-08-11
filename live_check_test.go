// live_check_test.go — the cloud-image catalogue against the real vendors.
//
// Gated behind VMX_CATALOG_LIVE=1: it makes six network requests, so it is
// not CI material, but it is the only check that catches the failure that
// actually happens — a vendor renaming an image or moving its checksum
// manifest, which turns every New VM build into "verification failed".
//
// Run it when touching cloudImages, and after any vendor release bump:
//
//	VMX_CATALOG_LIVE=1 go test -run TestLiveManifests -v
package main

import (
	"os"
	"path"
	"testing"
)

func TestLiveManifestsResolveEveryCatalogueImage(t *testing.T) {
	if os.Getenv("VMX_CATALOG_LIVE") != "1" {
		t.Skip("set VMX_CATALOG_LIVE=1 to check the catalogue against the vendors")
	}
	for _, distro := range CloudDistros() {
		ci := cloudImages[distro]
		m, err := fetchText(ci.SumURL)
		if err != nil {
			t.Errorf("%-7s manifest unreachable: %v", distro, err)
			continue
		}
		sum, err := expectedSum(m, path.Base(ci.URL))
		if err != nil {
			t.Errorf("%-7s %v", distro, err)
			continue
		}
		t.Logf("%-7s %s… (%s)", distro, sum[:16], ci.SumAlgo)
	}
}
