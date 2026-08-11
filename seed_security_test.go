package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The seed is an ISO9660 image that stays attached to the guest as a cdrom.
// Anything cleartext in it is readable both on the hypervisor and from inside
// the guest by any local user who can mount /dev/sr0. This is the assertion
// that keeps it out.
func TestSeedNeverCarriesTheCleartextPassword(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl to hash with; the cleartext fallback is expected here")
	}
	const secret = "sup3rs3cr3t-not-in-the-seed"
	ud := userData(NewVMSpec{Name: "t", User: "admin", Password: secret})

	if strings.Contains(ud, secret) {
		t.Fatalf("cleartext password present in the seed:\n%s", ud)
	}
	if !strings.Contains(ud, "type: hash") {
		t.Errorf("password not written as a hash:\n%s", ud)
	}
	if !strings.Contains(ud, "$6$") {
		t.Errorf("expected a SHA-512 crypt hash in the seed:\n%s", ud)
	}
}

// The fallback exists so a host with no hashing tool still produces a VM
// somebody can log into — but it must announce itself, or a cleartext seed
// ships silently.
func TestCleartextFallbackAnnouncesItself(t *testing.T) {
	h, err := hashPassword("x")
	if err != nil {
		t.Skip("no hashing tool here, which is the case the fallback covers")
	}
	if !strings.HasPrefix(h, "$6$") {
		t.Errorf("hashPassword returned %q, want a $6$ crypt hash", h)
	}
}

// A key-only guest gets no password at all: nothing to leak.
func TestKeyOnlyGuestHasNoPasswordInTheSeed(t *testing.T) {
	ud := userData(NewVMSpec{
		Name: "t", User: "admin",
		SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@example",
	})
	if strings.Contains(ud, "chpasswd") {
		t.Errorf("key-only guest should carry no password block:\n%s", ud)
	}
}
