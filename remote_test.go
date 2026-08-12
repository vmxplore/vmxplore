package main

import "testing"

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in       string
		wantURI  string
		wantSSH  string
		wantHost string
	}{
		{"", "qemu:///system", "", "local"},
		{"local", "qemu:///system", "", "local"},
		{"fiend.unixbox.net", "qemu+ssh://fiend.unixbox.net/system",
			"fiend.unixbox.net", "fiend.unixbox.net"},
		{"admin@fiend", "qemu+ssh://admin@fiend/system", "admin@fiend", "fiend"},
		{"qemu+ssh://root@box/system", "qemu+ssh://root@box/system",
			"root@box", "box"},
		{"qemu:///system", "qemu:///system", "", "local"},
	}
	for _, c := range cases {
		got := ParseTarget(c.in)
		if got.LibvirtURI != c.wantURI || got.SSHHost != c.wantSSH ||
			got.Host != c.wantHost {
			t.Errorf("ParseTarget(%q) = %+v, want uri=%q ssh=%q host=%q",
				c.in, got, c.wantURI, c.wantSSH, c.wantHost)
		}
	}
}

// virsh() and zfsArgv() must route through the target.
func TestArgvRouting(t *testing.T) {
	defer func() { target = Target{LibvirtURI: "qemu:///system", Host: "local"} }()

	target = ParseTarget("local")
	if v := virsh("start", "x"); v[2] != "qemu:///system" {
		t.Errorf("local virsh uri = %q", v[2])
	}
	if z := zfsArgv("list"); z[0] != "zfs" {
		t.Errorf("local zfs = %v", z)
	}

	target = ParseTarget("admin@box")
	if v := virsh("start", "x"); v[2] != "qemu+ssh://admin@box/system" {
		t.Errorf("remote virsh uri = %q", v[2])
	}
	z := zfsArgv("list")
	if z[0] != "ssh" || z[len(z)-2] != "admin@box" || z[len(z)-1] != "'zfs' 'list'" {
		t.Errorf("remote zfs = %v", z)
	}
	// The policy flags are the point of sshArgv; a remote command that loses
	// BatchMode hangs a GUI on a hidden password prompt.
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=accept-new"} {
		found := false
		for _, a := range z {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("remote zfs argv missing %q: %v", want, z)
		}
	}
}

// A dataset or snapshot name is re-parsed by the remote login shell on the ssh
// path, so quoting is load-bearing, not cosmetic. This is the regression test
// for that: the metacharacters must survive as literals.
func TestRemoteArgsAreQuoted(t *testing.T) {
	defer func() { target = Target{LibvirtURI: "qemu:///system", Host: "local"} }()
	target = ParseTarget("admin@box")

	got := zfsArgv("snapshot", "rpool/vms/x@manual-a;reboot")
	remote := got[len(got)-1]
	if remote != `'zfs' 'snapshot' 'rpool/vms/x@manual-a;reboot'` {
		t.Errorf("remote command not quoted as one word each: %q", remote)
	}

	// The nastiest case: a name carrying a single quote must not be able to
	// close the quoting and start a command of its own.
	q := shellQuote("a'b")
	if q != `'a'\''b'` {
		t.Errorf("shellQuote(a'b) = %s", q)
	}
}

// validZFSName is the gate that keeps such a name from being constructed at
// all. Both halves matter: the local path is exec.Command (immune to shells)
// and must still reject nonsense ZFS itself would refuse.
func TestValidZFSName(t *testing.T) {
	for _, ok := range []string{"manual-20260811", "golden", "a_b.c:d-e", "20260811-143000"} {
		if err := validZFSName(ok); err != nil {
			t.Errorf("validZFSName(%q) rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "a b", "a;reboot", "a$(id)", "a`id`", "a|b", "a&b", "a>b",
		"a'b", `a"b`, "a\nb", "a*b", "a/b", "a@b", "../x",
	} {
		if err := validZFSName(bad); err == nil {
			t.Errorf("validZFSName(%q) accepted", bad)
		}
	}
}
