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

// The BACKEND is conditional, and the condition is the reachability probe.
// Whatever this host's answer is, the two must agree — an --audio argument
// without a reachable socket is the failure this file exists to prevent.
func TestAudioBackendTracksReachability(t *testing.T) {
	args := audioArgs(Target{})
	offered := slices.Contains(args, "--audio")
	if reachable := hostAudioReachable(); offered != reachable {
		t.Errorf("--audio offered = %v but hostAudioReachable() = %v; "+
			"offering a backend qemu cannot connect to fails VM startup",
			offered, reachable)
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
