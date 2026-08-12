// audio.go — giving a guest a sound card, and only wiring it to the host's
// speakers when that will actually work.
//
// What it does, in order:
//  1. audioArgs() decides what to append to a virt-install command line;
//  2. it always asks for an emulated sound device, because a guest with no
//     card at all cannot be fixed later without an edit-and-reboot;
//  3. it adds a host audio BACKEND only when the qemu process will be able to
//     reach the desktop user's PipeWire socket.
//
// WHY the second half is conditional and not just set: qemu treats an
// unreachable audio backend as fatal. Verified against qemu 10.2.2:
//
//	$ sudo -u qemu qemu-system-x86_64 -machine none -audiodev pipewire,id=snd0
//	qemu-system-x86_64: Failed to connect to PipeWire instance: Host is down
//	$ echo $?
//	1
//
// So an unconditional `--audio type=pipewire` on a qemu:///system connection
// does not degrade to a silent guest — it makes EVERY new VM fail to start.
// On a stock Fedora workstation that is the default state of the world:
// libvirt runs qemu as the unprivileged `qemu` user, and /run/user/$UID is
// mode 0700, so the socket inside it is unreachable no matter that the socket
// itself is world-writable.
//
// Notes:
//   - The check is a real access test as the qemu user, not a guess from file
//     modes: an ACL, a different qemu.conf `user`, or a system-wide PipeWire
//     all make it work, and none of them are visible in a stat() of the path.
//   - Being unable to reach PipeWire is NOT an error. The guest still gets its
//     card; only the wire to the host speakers is missing, and the operator is
//     told exactly how to add it (see audioHostHint).
package main

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"time"
)

// soundModel is the emulated card. ich9 (Intel HD Audio) is driven
// out-of-the-box by every guest OS this tool builds — Linux since forever,
// Windows since 7 — where the older ac97 needs a driver on modern Windows and
// virtio-sound needs a guest kernel new enough to have the driver at all.
const soundModel = "ich9"

// qemuUser is the account libvirt runs system-connection guests as. Matches
// the `user` setting in /etc/libvirt/qemu.conf, whose default is "qemu" on
// every distro in the matrix.
const qemuUser = "qemu"

// audioProbeTimeout bounds the reachability test. It runs on the path that
// builds a VM, so it must never be the reason a build appears to hang.
const audioProbeTimeout = 3 * time.Second

// hostAudioReachable reports whether a guest built now would get working
// host audio.
//
// Returns: true only when qemu ITSELF, running as the account libvirt will
// use, successfully opens the backend. Every uncertainty — no runtime dir, no
// such user, no qemu binary, sudo unavailable, probe timed out — returns
// false, because the cost of a false positive is a VM that will not start and
// the cost of a false negative is a guest with a silent sound card.
//
// WHY this asks qemu instead of checking the socket: filesystem access is
// necessary but nowhere near sufficient. Measured on this host — granting
// `setfacl -m u:qemu:x /run/user/1000` plus `u:qemu:rw` on the socket makes
// `test -r` succeed, and qemu STILL exits 1 with "Failed to connect to
// PipeWire instance: Host is down", with or without XDG_RUNTIME_DIR exported.
// PipeWire refuses the client for reasons a stat() cannot see. A permission
// check would therefore have reported "reachable" in exactly the state that
// breaks every build, which is the failure this whole file exists to avoid.
// The only honest test of "can qemu connect" is to have qemu try.
//
// PERF: one short-lived `-machine none` qemu per call, ~100ms, on the path
// that builds a VM — a build that takes tens of seconds at minimum. Not worth
// caching; a cached "yes" that goes stale when the session restarts is the
// same false positive in slow motion.
func hostAudioReachable() bool {
	if pipewireSocket() == "" {
		return false
	}
	if _, err := user.Lookup(qemuUser); err != nil {
		return false
	}
	qemuBin, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), audioProbeTimeout)
	defer cancel()
	// -machine none so nothing is emulated but the audio backend, and no
	// monitor or serial so it cannot wait for anyone. qemu exits 0 when the
	// backend opens and 1 when it cannot.
	cmd := exec.CommandContext(ctx, "sudo", "-n", "-u", qemuUser, qemuBin,
		"-machine", "none", "-audiodev", "pipewire,id=probe",
		"-monitor", "none", "-serial", "none")
	return cmd.Run() == nil
}

// pipewireSocket returns the path to this session's PipeWire socket, or "" if
// there is no reason to believe one exists.
//
// XDG_RUNTIME_DIR is preferred over a constructed path because a session that
// sets it somewhere unusual is exactly the session whose audio we would
// otherwise wire to the wrong place.
func pipewireSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	sock := dir + "/pipewire-0"
	if _, err := os.Stat(sock); err != nil {
		return ""
	}
	return sock
}

// audioArgs returns the virt-install arguments that give a guest sound.
//
// Args:    tgt — where the guest will actually run.
// Returns: always a sound device; additionally a pipewire backend, but ONLY
//
//	for a guest running on this machine. Never returns an error — a
//	host with no audio is a normal host, not a broken one.
//
// WARN: the backend is local-only, and the reason is easy to miss. virt-install
// runs on THIS machine even when it is building a guest somewhere else — the
// destination is `--connect qemu+ssh://…`, not a different process. So the
// probe in hostAudioReachable() answers a question about the wrong computer
// whenever the target is remote: a laptop with working local audio would
// cheerfully add --audio to a VM whose qemu lives on a server that has no
// audio session at all, and qemu exits 1 rather than starting it silently.
// A remote guest's audio would come out of the remote machine's speakers
// regardless, so there is nothing lost by never asking.
//
// Example: on a local desktop wired for audio this yields
//
//	--sound ich9 --audio type=pipewire
//
// and on a stock system connection, or any remote target, just
//
//	--sound ich9
func audioArgs(tgt Target) []string {
	args := []string{"--sound", soundModel}
	// SSHHost is empty exactly when the target is this machine (see Target).
	if tgt.SSHHost == "" && hostAudioReachable() {
		args = append(args, "--audio", "type=pipewire")
	}
	return args
}

// audioHostHint is the one-line fix for a host whose qemu user cannot reach
// PipeWire, phrased so it can be pasted. Shown rather than performed: it
// grants another account access to the desktop session's audio, which is the
// operator's call and not a side effect of building a VM.
//
// The ACL lives on the runtime directory, which is tmpfs and rebuilt at every
// login, so a durable fix needs it reapplied per session — a systemd --user
// unit, not a one-shot command.
func audioHostHint() string {
	sock := pipewireSocket()
	if sock == "" {
		return "no PipeWire socket in this session: guest sound cards will " +
			"be silent"
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	// WARN: the ACL alone is NOT enough — measured on Fedora 44 with
	// PipeWire, granting the qemu user access to the socket still leaves
	// qemu unable to connect ("Host is down"). Setting `user = "<you>"` in
	// /etc/libvirt/qemu.conf, so guests run as the account that owns the
	// audio session, is the route that actually works; it also drops the
	// isolation between guests and the desktop user, which is why this is
	// printed for a human to decide rather than applied.
	return "guest audio is silent: qemu (running as " + qemuUser +
		") cannot open " + sock + ". Granting an ACL on " + dir +
		" is not sufficient on PipeWire; set user=\"" +
		os.Getenv("USER") + "\" in /etc/libvirt/qemu.conf and restart " +
		"libvirtd so guests run as the owner of the audio session"
}
