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
	"bufio"
	"context"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// soundModel is the emulated card. ich9 (Intel HD Audio) is driven
// out-of-the-box by every guest OS this tool builds — Linux since forever,
// Windows since 7 — where the older ac97 needs a driver on modern Windows and
// virtio-sound needs a guest kernel new enough to have the driver at all.
const soundModel = "ich9"

// defaultQemuUser is the account libvirt runs system-connection guests as
// when qemu.conf does not say otherwise. "qemu" on every distro in the matrix.
const defaultQemuUser = "qemu"

// qemuConfPath is where the `user` setting lives. World-readable on every
// distro here, which is why this can be parsed without privilege.
const qemuConfPath = "/etc/libvirt/qemu.conf"

// libvirtQemuUser returns the account guests will actually run as.
//
// Returns: the configured user, or defaultQemuUser when the file is absent,
// unreadable or says nothing.
//
// WHY this is read rather than assumed: giving guests access to the host's
// audio session is done by setting `user` here, so the very configuration
// that makes audio work is the one that moves the account out from under a
// hardcoded "qemu". A probe testing the wrong account reports "unreachable"
// on exactly the hosts where it now works, and the operator sees no sound
// after doing the thing that enables sound.
//
// The LAST uncommented assignment wins, matching libvirt's own parser: the
// file ships with commented examples and operators append rather than edit.
func libvirtQemuUser() string {
	f, err := os.Open(qemuConfPath)
	if err != nil {
		// /etc/libvirt is mode 0700 on every distro here, so this is the
		// NORMAL path for an unprivileged GUI — not an edge case. Falling
		// straight through to the default would answer "qemu" on a host
		// configured otherwise, probe the wrong account, and disable audio
		// on exactly the machines where it works. A running guest is ground
		// truth and costs nothing to read.
		if who := runningQemuOwner(); who != "" {
			return who
		}
		return defaultQemuUser
	}
	defer f.Close()
	user := defaultQemuUser
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rest, ok := strings.CutPrefix(line, "user")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest, ok = strings.CutPrefix(rest, "="); !ok {
			continue
		}
		if v := strings.Trim(strings.TrimSpace(rest), `"`); v != "" {
			user = v
		}
	}
	return user
}

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
	who := libvirtQemuUser()
	if _, err := user.Lookup(who); err != nil {
		return false
	}
	qemuBin, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), audioProbeTimeout)
	defer cancel()
	// -machine none emulates nothing but the audio backend. The monitor on
	// stdin is what makes this answerable: qemu that OPENS the backend goes
	// on running happily and never exits, so an earlier version of this
	// could only ever observe failure — success looked identical to a hung
	// probe and timed out into a false negative, leaving the backend off on
	// exactly the hosts where it works. Feeding "quit" to the monitor makes
	// the success path exit 0; a backend that cannot open still exits 1
	// before it ever reads stdin.
	probe := []string{"-machine", "none", "-display", "none",
		"-monitor", "stdio", "-audiodev", "pipewire,id=probe"}
	var cmd *exec.Cmd
	if cur, err := user.Current(); err == nil && cur.Username == who {
		// Guests already run as us: no sudo, and no passwordless-sudo
		// requirement just to answer a question about our own session.
		cmd = exec.CommandContext(ctx, qemuBin, probe...)
	} else {
		args := append([]string{"-n", "-u", who, qemuBin}, probe...)
		cmd = exec.CommandContext(ctx, "sudo", args...)
	}
	cmd.Stdin = strings.NewReader("quit\n")
	return cmd.Run() == nil
}

// runningQemuOwner returns the username a live guest's qemu is running as, or
// "" when no guest is running.
//
// Returns: the owner of the first qemu-system-* process found.
//
// WHY /proc rather than a config file: the config is root-only, and this is
// the answer it would have given anyway — with the config's own mistakes and
// any override already applied. It is only available while something is
// running, which is why it is a fallback and not the first choice.
func runningQemuOwner() string {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil || !strings.HasPrefix(string(comm), "qemu-system-") {
			continue
		}
		fi, err := os.Stat("/proc/" + e.Name())
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
		if err != nil {
			continue
		}
		return u.Username
	}
	return ""
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
	return "guest audio is silent: qemu (running as " + libvirtQemuUser() +
		") cannot open " + sock + ". Granting an ACL on " + dir +
		" is not sufficient on PipeWire; set user=\"" +
		os.Getenv("USER") + "\" in /etc/libvirt/qemu.conf and restart " +
		"libvirtd so guests run as the owner of the audio session"
}
