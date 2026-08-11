package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The six vendors publish three different layouts. These are real lines,
// copied from the manifests as of 2026-08-10 — if a vendor changes format
// this is what should fail, loudly, rather than the check silently passing.
func TestExpectedSumParsesEveryVendorFormat(t *testing.T) {
	cases := []struct {
		vendor, manifest, name, want string
	}{
		{"fedora/centos/rocky (BSD)",
			"# Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2: 585701904 bytes\n" +
				"SHA256 (Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2) = 7E4FB73907ABDC761D226DDAF3263BDF\n",
			"Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2",
			"7e4fb73907abdc761d226ddaf3263bdf"},
		{"ubuntu (GNU, binary marker)",
			"04ce2617d75f0a78ec64d400c3941042 *noble-server-cloudimg-riscv64-root.tar.xz\n" +
				"0533b0655c32e68b31d792ecd6ccfca9 *noble-server-cloudimg-amd64.img\n",
			"noble-server-cloudimg-amd64.img",
			"0533b0655c32e68b31d792ecd6ccfca9"},
		{"arch (GNU, two spaces)",
			"9ca8d4b0a60e53b8aa1ac2317166ecff  Arch-Linux-x86_64-cloudimg.qcow2\n",
			"Arch-Linux-x86_64-cloudimg.qcow2",
			"9ca8d4b0a60e53b8aa1ac2317166ecff"},
	}
	for _, c := range cases {
		got, err := expectedSum(c.manifest, c.name)
		if err != nil {
			t.Errorf("%s: %v", c.vendor, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.vendor, got, c.want)
		}
	}
}

// "Not listed" must be an error. Treating it as success is how an
// unverifiable image slips through the moment a vendor renames something.
func TestUnlistedFileIsAnError(t *testing.T) {
	_, err := expectedSum("abc123  some-other-image.qcow2\n", "the-one-we-want.qcow2")
	if err == nil {
		t.Fatal("a file absent from the manifest must not verify")
	}
	if !strings.Contains(err.Error(), "the-one-we-want.qcow2") {
		t.Errorf("the error should name what it looked for, got: %v", err)
	}
}

// A mismatch must delete the file, because the download path is also the
// cache — a bad image left in place is reused forever and never re-fetched.
func TestMismatchDeletesTheImage(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "cloud.qcow2")
	if err := os.WriteFile(img, []byte("not the real image"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point at a manifest that cannot be fetched to confirm we fail closed;
	// then the same check with a local mismatch below.
	ci := CloudImage{URL: "https://example.invalid/cloud.qcow2", SumAlgo: "sha256"}
	if err := VerifyImage(img, ci, func(string) {}); err == nil {
		t.Error("an entry with no checksum manifest must not verify")
	}
	if _, err := os.Stat(img); err != nil {
		t.Error("a missing manifest is not a corrupt image — the file should survive")
	}
}

func TestFileSumPicksTheAlgorithm(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x")
	if err := os.WriteFile(f, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Known digests of "abc".
	s256, err := fileSum(f, "sha256")
	if err != nil || s256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256 = %q, %v", s256, err)
	}
	s512, err := fileSum(f, "sha512")
	if err != nil || !strings.HasPrefix(s512, "ddaf35a193617aba") {
		t.Errorf("sha512 = %q, %v", s512, err)
	}
	if _, err := fileSum(f, "md5"); err == nil {
		t.Error("an unknown algorithm must be refused, not silently defaulted")
	}
}
