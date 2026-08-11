package main

import (
	"strings"
	"testing"
)

func TestRHELRegistrationIsOptional(t *testing.T) {
	if got := rhelPostInstall(NewVMSpec{Name: "x"}); got != "" {
		t.Errorf("no credentials must produce no registration block, got:\n%s", got)
	}
}

func TestRHELAcceptsBothAuthMethods(t *testing.T) {
	u := rhelPostInstall(NewVMSpec{RHELUser: "me@example.com", RHELPass: "pw"})
	if !strings.Contains(u, "--username") || !strings.Contains(u, "--auto-attach") {
		t.Errorf("portal login should register and attach:\n%s", u)
	}
	k := rhelPostInstall(NewVMSpec{RHELKey: "mykey", RHELOrg: "12345"})
	if !strings.Contains(k, "--activationkey") || !strings.Contains(k, "--org") {
		t.Errorf("activation key should register by key and org:\n%s", k)
	}
	// Half a credential pair is not a credential.
	if rhelPostInstall(NewVMSpec{RHELUser: "me"}) != "" {
		t.Error("a username with no password must not produce a register call")
	}
	if rhelPostInstall(NewVMSpec{RHELKey: "k"}) != "" {
		t.Error("a key with no org must not produce a register call")
	}
}

// Operator-supplied text lands inside a generated bash script. A quote in a
// password must be a password, not shell syntax.
func TestCredentialsAreShellQuoted(t *testing.T) {
	out := rhelPostInstall(NewVMSpec{RHELUser: "a'b", RHELPass: "p;rm -rf /"})
	// The whole value must sit inside one quoted word, so the semicolon is
	// password text rather than a command separator.
	if !strings.Contains(out, `--password 'p;rm -rf /'`) {
		t.Errorf("password must be a single quoted word:\n%s", out)
	}
	// An embedded quote closes, escapes, reopens — '\'' — so it can never
	// end the word early.
	if !strings.Contains(out, `--username 'a'\''b'`) {
		t.Errorf("embedded quote must be escaped:\n%s", out)
	}
}

// Registration has to precede the operator's script — that script will
// almost always try to install something, which needs repos.
func TestRegistrationRunsBeforeTheOperatorScript(t *testing.T) {
	ud := userData(NewVMSpec{
		Name: "x", User: "admin",
		RHELKey: "k", RHELOrg: "1",
		PostInst: "dnf install -y nginx",
	})
	reg := strings.Index(ud, "subscription-manager register")
	own := strings.Index(ud, "dnf install -y nginx")
	if reg < 0 || own < 0 {
		t.Fatalf("both blocks should be present:\n%s", ud)
	}
	if reg > own {
		t.Error("registration must come before the operator's own script")
	}
}
