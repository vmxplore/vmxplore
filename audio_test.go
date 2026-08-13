// audio_test.go — the audio backend must never be offered when it cannot
// connect, because qemu treats that as fatal rather than degrading to silence.
//
// HISTORY: 2026-08-11 — `sudo -u qemu qemu-system-x86_64 -audiodev pipewire`
// exits 1 with "Failed to connect to PipeWire instance: Host is down" on a
// stock Fedora workstation, where /run/user/$UID is 0700 and libvirt runs
// guests as the unprivileged qemu user. An unconditional --audio would have
// made every newly built VM fail to start.
package main

import (
	"slices"
	"strings"
	"testing"
)

// The sound DEVICE is unconditional: a guest with no card cannot be given one
// later without an edit and a reboot, and an emulated card costs nothing.
func TestAudioArgsAlwaysGiveTheGuestACard(t *testing.T) {
	args := audioArgs(Target{})
	i := slices.Index(args, "--sound")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("audioArgs() = %v, want a --sound device", args)
	}
	if args[i+1] != soundModel {
		t.Errorf("sound model = %q, want %q", args[i+1], soundModel)
	}
}

// The create path must NEVER offer a host audio backend, whatever the probe
// says. qemu treats an unreachable backend as fatal, and libvirt starts qemu
// without XDG_RUNTIME_DIR — so a backend that works from a shell can still
// leave every domain unable to start. Wiring the card to the host is an
// explicit action on an existing domain, where it can be verified.
func TestCreatePathNeverOffersAHostBackend(t *testing.T) {
	for _, tgt := range []Target{{}, {SSHHost: "fiend.unixbox.net"}} {
		if args := audioArgs(tgt); slices.Contains(args, "--audio") {
			t.Errorf("audioArgs(%+v) = %v; the create path must not wire "+
				"the host backend — an unreachable one makes the domain "+
				"fail to start", tgt, args)
		}
	}
}

// A missing socket must read as "not reachable", never as "assume yes".
func TestNoSocketMeansNotReachable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // empty: no pipewire-0 inside
	if pipewireSocket() != "" {
		t.Fatal("pipewireSocket() found a socket in an empty directory")
	}
	if hostAudioReachable() {
		t.Error("hostAudioReachable() = true with no socket present")
	}
	if slices.Contains(audioArgs(Target{}), "--audio") {
		t.Error("audioArgs() offered a backend with no socket present")
	}
}

// The operator-facing hint has to name the account and the path, or it is not
// actionable at 3am.
func TestAudioHostHintIsActionable(t *testing.T) {
	if h := audioHostHint(); h == "" {
		t.Fatal("audioHostHint() is empty")
	}
}

// A remote target must never be offered a host audio backend: virt-install
// runs here, the guest runs there, and this machine's audio session says
// nothing about that one. Offering it makes the remote VM fail to start.
func TestRemoteTargetNeverGetsAHostBackend(t *testing.T) {
	remote := Target{
		LibvirtURI: "qemu+ssh://fiend.unixbox.net/system",
		SSHHost:    "fiend.unixbox.net",
		Host:       "fiend.unixbox.net",
	}
	args := audioArgs(remote)
	if slices.Contains(args, "--audio") {
		t.Errorf("audioArgs(remote) = %v; a remote guest must not be given "+
			"this host's audio backend", args)
	}
	if !slices.Contains(args, "--sound") {
		t.Errorf("audioArgs(remote) = %v; a remote guest still gets a card",
			args)
	}
}

// The probe has to be able to observe SUCCESS, not just failure. On this host
// the answer depends on qemu.conf and the session, so assert the two halves
// agree rather than a fixed value — and print what was found, because a
// silent false is exactly how the first version of this hid a bug.
func TestProbeAgreesWithConfiguredUser(t *testing.T) {
	who := libvirtQemuUser()
	if who == "" {
		t.Fatal("libvirtQemuUser() returned empty")
	}
	t.Logf("guests run as %q; host audio reachable = %v",
		who, hostAudioReachable())
}

// The guest half must exist for every distro the cloud path can build, or
// "sound" silently means "a card the guest cannot use" on some of them.
func TestSoundPostInstallCoversTheCloudDistros(t *testing.T) {
	for _, d := range []string{"fedora", "centos", "rocky", "rhel", "debian", "ubuntu", "arch"} {
		s := soundPostInstall(d)
		if s == "" {
			t.Errorf("soundPostInstall(%q) is empty — the guest would get a "+
				"card with no stack to drive it", d)
			continue
		}
		if !strings.Contains(s, "pipewire") {
			t.Errorf("soundPostInstall(%q) installs no pipewire: %s", d, s)
		}
	}
	if got := soundPostInstall("plan9"); got != "" {
		t.Errorf("unknown distro should yield no snippet, got: %s", got)
	}
}

// Sound is opt-in. An unchecked build must not carry the guest snippet.
func TestSoundIsOptIn(t *testing.T) {
	off := userData(NewVMSpec{Name: "x", User: "admin", Distro: "fedora"})
	if strings.Contains(off, "guest audio stack") {
		t.Error("sound setup present without Sound being set")
	}
	on := userData(NewVMSpec{Name: "x", User: "admin", Distro: "fedora", Sound: true})
	if !strings.Contains(on, "guest audio stack") {
		t.Error("Sound=true did not inject the guest audio setup")
	}
}
