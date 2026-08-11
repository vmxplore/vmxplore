// verify.go — prove a downloaded cloud image is the one the vendor published.
//
// What it does, in order:
//  1. Fetches the vendor's checksum manifest that sits beside the image.
//  2. Finds the line for this image's filename, in either of the two formats
//     the six vendors actually use.
//  3. Hashes the file on disk and compares.
//  4. On mismatch, DELETES the file before returning the error.
//
// WHY: New VM downloaded a multi-gigabyte disk image as root, over the
// network, and wrote it straight onto a zvol that a guest then booted. The
// appliance catalogue verified its downloads with sha256sum -c; the base
// images those appliances run on got nothing. A corrupt mirror, a truncated
// transfer or a hostile one all produced a VM that looked fine.
//
// WHY the manifest rather than a pinned digest: every image URL in the
// catalogue is a "latest" pointer that moves when the vendor rebuilds. A
// digest hardcoded here would be wrong within days, and the pressure would
// be to delete the check rather than chase it. Fetching the manifest beside
// the image verifies the bytes actually published for that URL, and survives
// the rebuild.
//
// What this is NOT: a signature check. The manifest travels over the same
// TLS connection as the image, so this catches corruption, truncation and a
// broken mirror — not a vendor whose infrastructure is compromised. Several
// of these manifests are GPG-signed and checking those signatures is the
// next step up; it needs each vendor's key, which is a bigger dependency
// than this pass takes.
//
// Notes: step 4 matters more than it looks. The image path is also the
// CACHE, so leaving a failed download in place means every later VM build
// reuses the bad bytes and skips the download entirely.
package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// bsdSumRE matches the BSD-style line Fedora, CentOS and Rocky publish:
//
//	SHA256 (Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2) = 7e4fb739…
var bsdSumRE = regexp.MustCompile(`^\s*SHA(?:256|512)\s*\(([^)]+)\)\s*=\s*([0-9a-fA-F]+)\s*$`)

// gnuSumRE matches the GNU coreutils style Debian, Ubuntu and Arch publish:
//
//	0533b0655c32…  noble-server-cloudimg-amd64.img
//	0533b0655c32… *noble-server-cloudimg-amd64.img   (binary mode)
var gnuSumRE = regexp.MustCompile(`^\s*([0-9a-fA-F]{32,})\s+[* ]?(\S+)\s*$`)

// expectedSum finds the digest for one filename in a checksum manifest.
//
// Args:    manifest  the whole file as text
//
//	name      the image's base filename
//
// Returns: the lowercase hex digest, or an error naming what it looked for.
// Failure modes callers must handle: the vendor renamed the file, or moved
// to a format neither regexp covers — both mean "cannot verify", which the
// caller must treat as failure rather than success.
func expectedSum(manifest, name string) (string, error) {
	for _, line := range strings.Split(manifest, "\n") {
		if m := bsdSumRE.FindStringSubmatch(line); m != nil {
			// Vendors list bare names; compare on the base either way so a
			// manifest that ever starts using ./path still matches.
			if path.Base(m[1]) == name {
				return strings.ToLower(m[2]), nil
			}
			continue
		}
		if m := gnuSumRE.FindStringSubmatch(line); m != nil {
			if path.Base(m[2]) == name {
				return strings.ToLower(m[1]), nil
			}
		}
	}
	return "", fmt.Errorf("%s is not listed in the vendor's checksum manifest", name)
}

// fileSum hashes a file with the named algorithm, streaming so a 3GB image
// never lands in memory.
func fileSum(path, algo string) (string, error) {
	var h hash.Hash
	switch algo {
	case "sha512":
		h = sha512.New()
	case "sha256", "":
		h = sha256.New()
	default:
		return "", fmt.Errorf("unknown checksum algorithm %q", algo)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// fetchText retrieves a checksum manifest. Small file, short timeout, and a
// size cap so a redirect to something enormous cannot hang a VM build.
func fetchText(url string) (string, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(b), err
}

// VerifyImage checks a downloaded image against its vendor manifest and
// removes it if it does not match.
//
// Args:    file      the downloaded image on disk
//
//	ci        its catalogue entry, carrying SumURL and SumAlgo
//	progress  narration, same callback the build steps use
//
// Returns: nil when the bytes match what the vendor published.
// Failure modes callers must handle: an entry with no SumURL returns an
// error rather than passing silently — an unverifiable image must be a
// visible decision, not an accident. The caller may choose to proceed, but
// it has to say so.
func VerifyImage(file string, ci CloudImage, progress func(string)) error {
	name := path.Base(ci.URL)
	if ci.SumURL == "" {
		return fmt.Errorf("no checksum manifest known for %s — cannot verify", name)
	}
	progress("verifying " + name + " against the vendor checksum")
	manifest, err := fetchText(ci.SumURL)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", ci.SumURL, err)
	}
	want, err := expectedSum(manifest, name)
	if err != nil {
		return err
	}
	got, err := fileSum(file, ci.SumAlgo)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", file, err)
	}
	if got != want {
		// The download path IS the cache. Leaving a bad image here means
		// every later build reuses it and skips the download entirely, so
		// one corrupt transfer would poison every VM from then on.
		_ = os.Remove(file)
		return fmt.Errorf(
			"%s does not match the vendor checksum (deleted it)\n  want %s\n  got  %s",
			name, want, got)
	}
	progress("verified " + name)
	return nil
}
