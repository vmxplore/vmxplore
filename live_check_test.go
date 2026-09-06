// live_check_test.go — the cloud-image catalogue against the real vendors.
//
// Gated behind VMX_CATALOG_LIVE=1: it makes a few dozen network requests, so it is
// not CI material, but it is the only check that catches the failure that
// actually happens — a vendor renaming an image or moving its checksum
// manifest, which turns every New VM build into "verification failed".
//
// Run it when touching cloudImages, and after any vendor release bump:
//
//	VMX_CATALOG_LIVE=1 go test -run TestLiveManifests -v
package main

import (
	"net/http"
	"os"
	"path"
	"testing"
	"time"
)

func TestLiveManifestsResolveEveryCatalogueImage(t *testing.T) {
	if os.Getenv("VMX_CATALOG_LIVE") != "1" {
		t.Skip("set VMX_CATALOG_LIVE=1 to check the catalogue against the vendors")
	}
	for _, distro := range CloudDistros() {
		ci, err := resolveCloudImage(cloudImages[distro])
		if err != nil {
			t.Errorf("%-20s %v", distro, err)
			continue
		}
		m, err := fetchText(ci.SumURL)
		if err != nil {
			t.Errorf("%-20s manifest unreachable: %v", distro, err)
			continue
		}
		sum, err := expectedSum(m, path.Base(ci.URL))
		if err != nil {
			t.Errorf("%-20s %v", distro, err)
			continue
		}
		// The manifest can be fine while the image itself is gone — Amazon's
		// pinned build returned 403 beside a healthy SHA256SUMS. Ask for
		// the file, not just its hash.
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Head(ci.URL)
		if err != nil {
			t.Errorf("%-20s image HEAD failed: %v", distro, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%-20s image %s returned %s", distro, ci.URL, resp.Status)
			continue
		}
		t.Logf("%-20s %s… (%s)  %s", distro, sum[:16], ci.SumAlgo, path.Base(ci.URL))
	}
}
