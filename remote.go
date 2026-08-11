// remote.go — the connection target: local or a remote host over ssh.
//
// vmxplore reads the estate through go-libvirt (which speaks qemu+ssh://
// natively over a pure-Go dialer — no cgo, the static story holds). But
// the verbs, the consoles and the ZFS join shell out to virsh/zfs/vnc, and
// those must run WHERE THE HYPERVISOR IS. This file centralizes that: one
// Target, set at connect, that every exec path consults.
//
//	local:   virsh -c qemu:///system … ; zfs … ; vnc 127.0.0.1:port
//	remote:  virsh -c qemu+ssh://host/system … ; ssh host zfs … ;
//	         vnc through an ssh -L tunnel to the host's loopback
//
// Why one global rather than threading a handle: the whole GUI/TUI is a
// single connection at a time; a global set once at connect keeps the verb
// plan builders pure (they call virsh()/zfsArgv() without carrying state)
// and is trivially correct for the one-connection model. Reconnecting to a
// different host re-sets it.
//
// Notes: the ssh host is parsed from the libvirt URI's user@host, so a
// working `virsh -c qemu+ssh://…` implies working `ssh host` — same key,
// same known_hosts. That is also what carries the console: guests bind VNC
// to loopback, and a remote console is an ssh -L forward to it.
//
// HISTORY 2026-08-10: guests were created with `--graphics vnc,listen=
// 0.0.0.0` and the console dialled the host's VNC port over plain TCP. RFB
// with security type None means anyone who could reach that port had
// keyboard and mouse on the guest, unauthenticated — and the README said
// the remote console ran over ssh, which it did not. Guests now bind
// 127.0.0.1 and the tunnel makes the README true.
package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// reexec replaces the current process with argv[0] argv[1:], inheriting the
// environment — used by the GUI's Connect button to relaunch pointed at a
// different host (a live reconnect would have to rebuild every pane).
func reexec(exe string, argv []string) error {
	return syscall.Exec(exe, argv, os.Environ())
}

// Target is where operations run.
type Target struct {
	LibvirtURI string // qemu:///system  |  qemu+ssh://user@host/system
	SSHHost    string // "" local; "user@host" remote (zfs, future tunnels)
	Host       string // display: "local" or "host"
}

// target is the process-wide connection, set by ConnectTarget. Defaults to
// local so non-GUI paths and tests need no setup.
var target = Target{LibvirtURI: "qemu:///system", Host: "local"}

// ParseTarget turns a user-typed destination into a Target. Accepts a bare
// host ("fiend.unixbox.net" → qemu+ssh://fiend.unixbox.net/system), a
// user@host, or a full libvirt URI. Empty or "local" stays local.
func ParseTarget(s string) Target {
	s = strings.TrimSpace(s)
	if s == "" || s == "local" || s == "qemu:///system" {
		return Target{LibvirtURI: "qemu:///system", Host: "local"}
	}
	if strings.Contains(s, "://") {
		t := Target{LibvirtURI: s, Host: s}
		if u, err := url.Parse(s); err == nil && strings.Contains(u.Scheme, "ssh") {
			t.SSHHost = u.Host // user@host:port form preserved by url.User+Host
			if u.User != nil {
				t.SSHHost = u.User.Username() + "@" + u.Host
			}
			t.Host = u.Hostname()
		}
		return t
	}
	// bare host or user@host → ssh transport to /system
	host := s
	if i := strings.IndexByte(s, '@'); i >= 0 {
		host = s[i+1:]
	}
	return Target{
		LibvirtURI: "qemu+ssh://" + s + "/system",
		SSHHost:    s,
		Host:       host,
	}
}

// virsh builds a virsh argv pinned to the target's libvirt URI — the
// connection the whole tool reads from. Bare virsh as a non-root
// libvirt-group user defaults to qemu:///session (an empty estate); the
// explicit -c also carries us to a remote host unchanged.
func virsh(args ...string) []string {
	return append([]string{"virsh", "-c", target.LibvirtURI}, args...)
}

// zfsArgv builds a zfs argv that runs on the hypervisor: locally when the
// target is local, over ssh when remote (the pool lives on the remote box,
// not here).
func zfsArgv(args ...string) []string {
	if target.SSHHost == "" {
		return append([]string{"zfs"}, args...)
	}
	return append([]string{"ssh", target.SSHHost, "zfs"}, args...)
}

// vncDialHost is where the RFB client connects for a guest's VNC port:
// loopback locally, the hypervisor host when remote.
func vncDialHost() string {
	if target.Host == "local" || target.SSHHost == "" {
		return "127.0.0.1"
	}
	h := target.Host
	if i := strings.IndexByte(target.SSHHost, '@'); i >= 0 {
		h = target.SSHHost[i+1:]
	}
	// strip any :port from the ssh host spec
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return h
}

// ─── console transport ───────────────────────────────────────────────────

// vncEndpoint returns the address the RFB client should dial for a guest's
// VNC port, plus a function to call when the console closes.
//
// Local: the guest binds 127.0.0.1 and we dial it directly; the returned
// closer is a no-op.
//
// Remote: the guest also binds 127.0.0.1 — on the HYPERVISOR's loopback,
// which is unreachable from here by design. An `ssh -L` forward makes it
// reachable to this process only, over the same authenticated channel that
// libvirt is already using. The closer tears the forward down; leaking one
// per console open would leave a listening port for every VM ever viewed.
//
// Args:   port  the guest's VNC port, from the domain's live XML
// Returns: dial address, closer, error. The closer is always safe to call,
// including after an error return.
// Failure modes callers must handle: ssh missing, the forward refused
// (ExitOnForwardFailure makes that an error rather than a hang), or the
// forward not ready inside the timeout.
func vncEndpoint(port int) (string, func(), error) {
	noop := func() {}
	if target.Host == "local" || target.SSHHost == "" {
		return fmt.Sprintf("127.0.0.1:%d", port), noop, nil
	}

	// Bind :0 to have the kernel pick a free port, then release it. There is
	// a race between release and ssh binding it; it is small, and losing it
	// surfaces as a clean "forward failed" rather than a wrong connection,
	// because ExitOnForwardFailure makes ssh exit instead of continuing.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", noop, fmt.Errorf("no local port for the console tunnel: %w", err)
	}
	local := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// -N: set up the forward and no remote command. BatchMode: never sit at
	// a password prompt with no terminal to type into — fail instead.
	cmd := exec.Command("ssh", "-N",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", local, port),
		target.SSHHost)
	if err := cmd.Start(); err != nil {
		return "", noop, fmt.Errorf("ssh tunnel for the console failed to start: %w", err)
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}

	// ssh reports nothing on success, so readiness is "the forward accepts".
	addr := fmt.Sprintf("127.0.0.1:%d", local)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			return addr, stop, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	return "", noop, fmt.Errorf(
		"console tunnel to %s never came up — is the guest's VNC port %d open on its loopback?",
		target.SSHHost, port)
}
