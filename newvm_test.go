package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// userData must stay valid #cloud-config; the post-install block is the
// part most likely to break it (indentation inside the YAML scalar).
func TestUserDataPostInstall(t *testing.T) {
	s := NewVMSpec{
		Name: "web", User: "admin", Password: "x",
		PostInst: "dnf install -y nginx\nsystemctl enable --now nginx",
	}
	ud := userData(s)
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("must start with #cloud-config")
	}
	for _, want := range []string{
		"write_files:",
		"path: /var/lib/vmxplore-postinstall.sh",
		"      dnf install -y nginx", // 6-space block indent
		"      systemctl enable --now nginx",
		"runcmd:",
		"[ bash, /var/lib/vmxplore-postinstall.sh ]",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("post-install cloud-config missing %q in:\n%s", want, ud)
		}
	}
	// No operator post-install still emits a runcmd, because the guest agent
	// is enabled there on every build. What must NOT appear is the operator's
	// own script — the point of the original assertion was that an empty
	// post-install produces no user payload, and that still holds.
	bare := userData(NewVMSpec{Name: "n", User: "a"})
	if !strings.Contains(bare, "packages:") ||
		!strings.Contains(bare, "qemu-guest-agent") {
		t.Error("every build must install the guest agent")
	}
	if !strings.Contains(bare, "systemctl enable --now qemu-guest-agent") {
		t.Error("the guest agent must be enabled, not merely installed")
	}
	if strings.Contains(bare, "dnf install") ||
		strings.Contains(bare, "apt-get install") {
		t.Errorf("empty post-install must emit no operator payload:\n%s", bare)
	}
}

// waitZvolNode is the guard on the devtmpfs bug: qemu-img creates a plain
// file at a missing path, so a New VM that raced udev put the guest's
// whole disk in RAM. A non-device at the zvol path must be a hard error,
// never something to overwrite.
func TestWaitZvolNodeRejectsNonDevice(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fake-zvol")
	if err := os.WriteFile(f, []byte("not a block device"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := waitZvolNode(f, func(string) {})
	if err == nil {
		t.Fatal("accepted a regular file as a zvol node")
	}
	if !strings.Contains(err.Error(), "not a block device") {
		t.Errorf("error should name the cause, got: %v", err)
	}
}

// A path that never appears must time out with an actionable message
// rather than hanging or, worse, proceeding.
func TestWaitZvolNodeTimesOut(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-appears")
	start := time.Now()
	err := waitZvolNodeFor(missing, 300*time.Millisecond, func(string) {})
	if err == nil {
		t.Fatal("accepted a path that does not exist")
	}
	if !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("error should say the node never appeared, got: %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Error("returned without actually waiting")
	}
}

// Whatever /dev/zvol nodes this host already has must be accepted, so the
// guard cannot reject a legitimately-published zvol.
func TestWaitZvolNodeAcceptsRealZvol(t *testing.T) {
	// Datasets nest arbitrarily deep (rpool/vms/<name>), so try each depth
	// rather than assuming a layout.
	var matches []string
	for _, pat := range []string{"/dev/zvol/*/*", "/dev/zvol/*/*/*",
		"/dev/zvol/*/*/*/*"} {
		m, _ := filepath.Glob(pat)
		matches = append(matches, m...)
	}
	var dev string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Mode()&os.ModeDevice != 0 {
			dev = m
			break
		}
	}
	if dev == "" {
		t.Skip("no zvol block devices on this host")
	}
	if err := waitZvolNode(dev, func(string) {}); err != nil {
		t.Errorf("rejected real zvol %s: %v", dev, err)
	}
}

// TestUserDataAlwaysReachable guards the wf-desk incident (2026-08-09): the
// appliance dialog's guest-login fields were left empty, cloud-init made an
// account with no password and no key, and the finished VM could not be
// entered at the console or over ssh — only destroyed.
func TestUserDataAlwaysReachable(t *testing.T) {
	s := NewVMSpec{Name: "x", Distro: "debian", User: "admin",
		VCPUs: 1, RAMMB: 512, DiskGB: 4}
	ud := userData(s)
	// The password is hashed as of 2026-08-10, so "reachable" can no longer
	// be checked by looking for the cleartext — that is the whole point. It
	// is checked by the presence of the chpasswd block, which is what makes
	// the account usable.
	if !strings.Contains(ud, "chpasswd:") {
		t.Errorf("no password and no key must fall back to a default:\n%s", ud)
	}
	if strings.Contains(ud, DefaultGuestPassword) {
		t.Errorf("the default password must not appear in cleartext:\n%s", ud)
	}
	s.SSHKey = "ssh-ed25519 AAAA test"
	if strings.Contains(userData(s), "chpasswd:") {
		t.Error("a key on its own must not force a password")
	}
	s.SSHKey, s.Password = "", "hunter2"
	ud = userData(s)
	if !strings.Contains(ud, "chpasswd:") {
		t.Errorf("an explicit password must still produce a login:\n%s", ud)
	}
	if strings.Contains(ud, "hunter2") {
		t.Errorf("an explicit password must not be emitted in cleartext:\n%s", ud)
	}
}

// TestUserDataQuotesOperatorValues pins the wf-desktop incident
// (2026-08-09): an ssh key whose comment contained a colon —
// "ek-debug: dev login to appliances" — was emitted as a bare YAML
// scalar, parsed as a mapping, and cloud-init threw out the entire users
// block. The VM booted, the app worked, and the key was never installed.
func TestUserDataQuotesOperatorValues(t *testing.T) {
	key := "ssh-ed25519 AAAAC3Nz test ek-debug: dev login to appliances"
	s := NewVMSpec{Name: "x", Distro: "debian", User: "admin",
		Password: `p, w"d}`, SSHKey: key, VCPUs: 1, RAMMB: 512, DiskGB: 4}
	ud := userData(s)
	if !strings.Contains(ud, `- "`+key+`"`) {
		t.Errorf("ssh key must be a quoted scalar:\n%s", ud)
	}
	// The password is hashed now, so the value in the flow mapping is a
	// crypt string rather than the operator's text — but it must still be a
	// QUOTED scalar, because a crypt hash is full of $ and / and can end in
	// characters YAML would otherwise interpret. Quoting is the invariant
	// this test exists for; the cleartext was only ever the example.
	if strings.Contains(ud, `p, w"d}`) {
		t.Errorf("password must never appear in cleartext:\n%s", ud)
	}
	if !strings.Contains(ud, `password: "$6$`) {
		t.Errorf("hashed password must be a quoted scalar:\n%s", ud)
	}
	for _, line := range strings.Split(ud, "\n") {
		if strings.HasPrefix(line, "hostname:") && !strings.Contains(line, `"`) {
			t.Errorf("hostname must be quoted: %q", line)
		}
	}
}

// A preset with NameRE takes its filename from the vendor document: beside
// the manifest for a directory listing (Amazon), from the full href when the
// document carries one. A preset without NameRE is untouched.
func TestResolveCloudImageFrom(t *testing.T) {
	am := cloudImages["amazon"]
	got, err := resolveCloudImageFrom(am,
		"fd28073d294d4145e6a45ba4039b8e4405ec6f188758418a0a9155a8d5fe1c3f  al2023-kvm-2023.12.20260831.0-kernel-6.1-x86_64.xfs.gpt.qcow2\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://cdn.amazonlinux.com/al2023/os-images/latest/kvm/al2023-kvm-2023.12.20260831.0-kernel-6.1-x86_64.xfs.gpt.qcow2"; got.URL != want {
		t.Errorf("amazon URL = %s, want %s", got.URL, want)
	}
	// a document that carries full hrefs resolves to the href, not to the
	// manifest's directory
	href := CloudImage{SumURL: "https://example.org/list.html", NameRE: `thing-b[0-9]+\.qcow2`}
	got, err = resolveCloudImageFrom(href, `<a href="https://mirror.example.org/x86_64/thing-b7.qcow2">thing-b7.qcow2</a>`)
	if err != nil || got.URL != "https://mirror.example.org/x86_64/thing-b7.qcow2" {
		t.Errorf("href resolution: %v %s", err, got.URL)
	}
	// Alpine: the per-image .sha512 is the bare hash, nothing to name-match
	bare := "d0ddf1faae4d44aee3ad6621f166bd414c2f99b6974fb455408612d59cbb31a5390ca259800ac0c6b60505493880ed6de8beb986f6d0883b26c8e84ce75a266c\n"
	if got, err := expectedSum(bare, "nocloud_alpine-3.22.4-x86_64-bios-cloudinit-r0.qcow2"); err != nil || got != strings.TrimSpace(bare) {
		t.Errorf("bare-hash document: %v %q", err, got)
	}
	if _, err := expectedSum("not a hash at all\n", "x.qcow2"); err == nil {
		t.Error("a document that is neither a manifest nor a hash must be an error")
	}
	if _, err := resolveCloudImageFrom(am, "nothing here\n"); err == nil {
		t.Error("a document without a matching name must be an error, not a guess")
	}
	plain, err := resolveCloudImageFrom(cloudImages["fedora"], "")
	if err != nil || plain != cloudImages["fedora"] {
		t.Errorf("a pinned preset must pass through unchanged: %v %+v", err, plain)
	}
}
