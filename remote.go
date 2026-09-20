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
// working `virsh -c qemu+ssh://…` implies working `ssh host`. That is also
// what carries the console: guests bind VNC to loopback, and a remote console
// is an ssh -L forward to it.
//
// THE TRUST STORY, because a remote hypervisor is a security boundary and two
// different ssh implementations are in play here:
//
//   - The estate read is go-libvirt's own pure-Go ssh dialer. For the `ssh`
//     transport it verifies the host against ~/.ssh/known_hosts and FAILS
//     CLOSED on an unknown or changed key (only ?no_verify=1 in the URI opts
//     out). It does NOT read ~/.ssh/config, so a Host alias, IdentityFile,
//     User or ProxyJump there is invisible to it — it tries the agent, then
//     ~/.ssh/{identity,id_dsa,id_ecdsa,id_ed25519,id_rsa} in that order.
//   - Everything that shells out (zfs, the console's -L forward) is the system
//     ssh binary, which DOES read ~/.ssh/config — including a
//     StrictHostKeyChecking=no that a lab machine may have set globally. So
//     sshArgv sets the policy explicitly rather than inheriting whatever is
//     ambient: an unverified channel is not something to discover later, on
//     the connection that runs `zfs destroy -r`.
//
// The practical consequence of the asymmetry: if `ssh myhost` works only
// because of an ~/.ssh/config stanza, `--connect myhost` may still fail on
// authentication. Connect with the real user@hostname in that case.
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
	"net/url"
	"os"
	"os/exec"
	"strings"
)

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

// sshFlags is the policy every non-interactive ssh command in this tool runs
// under. There is exactly one place that builds an ssh argv for a command
// (sshArgv) and one for the console forward (vncEndpoint), and both use these,
// so the trust and timeout story cannot drift between them.
//
//	BatchMode=yes             — never sit at a password/passphrase prompt.
//	                            A GUI has no terminal to type into and a TUI
//	                            would hang mid-render; failing loudly is the
//	                            only honest outcome.
//	StrictHostKeyChecking=accept-new
//	                          — trust on first use, refuse a CHANGED key.
//	                            Not "no": the estate connection has already
//	                            verified this host against ~/.ssh/known_hosts
//	                            (see the banner), so in practice the key is
//	                            known by the time we get here and this only
//	                            stops an ambient StrictHostKeyChecking=no from
//	                            silently downgrading us.
//	ConnectTimeout=10         — a hypervisor that has gone away must not wedge
//	                            the 30s ZFS tick forever.
//
// Deliberately NOT set: ControlMaster/ControlPersist. Multiplexing would save
// a handshake on every tick, but a wedged master socket makes every later
// command hang on a connect that ConnectTimeout does not cover — trading a
// visible cost for an invisible failure mode.
var sshFlags = []string{
	"-o", "BatchMode=yes",
	"-o", "StrictHostKeyChecking=accept-new",
	"-o", "ConnectTimeout=10",
}

// sshGuestFlags is the policy for the INTERACTIVE ssh into a guest (the TUI's
// S key), and it is deliberately different from sshFlags above.
//
// No host-key checking: these guests are clones, minted with a fresh host key
// on every build, drawing from a small libvirt DHCP pool — so the same address
// is a different machine with a different key several times a day. The warning
// is correct every time and useful none of them, and a prompt in front of a
// key nobody can verify only teaches the operator to press yes unread. Both
// known-hosts files go to /dev/null so this neither writes an entry a later
// real connection would trip over, nor reads one.
//
// No BatchMode, unlike sshFlags: this one is attached to a terminal on
// purpose, and a guest that wants a password must be able to ask for it.
//
// This applies to guests on a private host bridge and nothing else. The
// hypervisor connection keeps StrictHostKeyChecking=accept-new, because a
// durable host whose key changes is news.
var sshGuestFlags = []string{
	"-o", "StrictHostKeyChecking=no",
	"-o", "UserKnownHostsFile=/dev/null",
	"-o", "GlobalKnownHostsFile=/dev/null",
	"-o", "LogLevel=ERROR", // without this, "Permanently added" on every connect
	"-o", "ConnectTimeout=10",
}

// guestKeys are the management keys this host may already hold, in the order
// worth trying. Every one of them was minted by a kldload tool and seeded into
// the guests it built, so offering them is what turns S from "type the
// password again" into a connection.
//
//	id_ed25519  — kube-cluster's cluster-management key. It writes the public
//	              half into every node's cloud-init ssh_authorized_keys, so a
//	              k8s node accepts it by construction.
//	id_kldload  — the estate key the installer lays down for klab and the
//	              appliance tiles.
//
// Missing files are skipped rather than passed: ssh treats an unreadable -i as
// an error worth printing, and a host that never built a cluster has no
// id_ed25519 to offer.
var guestKeys = []string{
	"/root/.ssh/id_ed25519",
	"/root/.ssh/id_kldload",
}

// sshGuestArgv builds the interactive guest ssh argv: the policy above, plus
// whichever management keys actually exist on this host, plus the destination.
//
// IdentitiesOnly is deliberately NOT set. These are ADDITIONAL candidates, not
// a replacement set — an operator with their own key in an agent, or in
// ~/.ssh/config for that host, keeps it, and the password prompt is still
// there when no key matches. Automating the common case must not remove the
// escape hatch for the uncommon one.
func sshGuestArgv(dest string) []string {
	argv := append([]string{}, sshGuestFlags...)
	for _, k := range guestKeys {
		if fi, err := os.Stat(k); err == nil && !fi.IsDir() {
			argv = append(argv, "-i", k)
		}
	}
	return append(argv, dest)
}

// sshArgv builds an ssh argv that runs one command on the target host.
//
// The remote command is passed as a SINGLE shell-quoted word, which is the
// whole point of this helper. ssh does not exec an argv on the far side: it
// joins the arguments with spaces and hands the result to the remote login
// shell, which re-parses it. So `ssh host zfs snapshot 'p/v@a;reboot'` runs
// reboot, while the identical local exec.Command argv cannot. Quoting here
// gives the remote path the same "argv, never a shell string" property the
// local path has by construction.
//
// Args:    host — user@host; cmd — the argv to run there.
// Returns: an argv for exec.Command.
func sshArgv(host string, cmd ...string) []string {
	argv := append([]string{"ssh"}, sshFlags...)
	return append(argv, host, shellQuoteArgv(cmd...))
}

// shellQuoteArgv renders an argv as one shell command string that survives
// re-parsing on the far side: every element single-quoted (shellQuote, shared
// with the post-installer generator in newvm.go), joined by spaces. Nothing is
// special inside single quotes, so this is total rather than a blacklist of
// metacharacters — which is the property that makes the remote path as safe as
// exec.Command is locally.
func shellQuoteArgv(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// zfsArgv builds a zfs argv that runs on the hypervisor: locally when the
// target is local, over ssh when remote (the pool lives on the remote box,
// not here). Remote args are quoted by sshArgv — see the note there for why
// that is a safety property and not a formality.
func zfsArgv(args ...string) []string {
	if target.SSHHost == "" {
		return append([]string{"zfs"}, args...)
	}
	return sshArgv(target.SSHHost, append([]string{"zfs"}, args...)...)
}

// validZFSName gates every operator-typed name component before it can reach a
// zfs argv — a snapshot suffix or a clone name.
//
// The allowed set is ZFS's own for a dataset/snapshot component: alphanumerics
// plus _ - : . — so this rejects nothing ZFS would have accepted. It is an
// allowlist rather than a check for spaces/@// because the local and remote
// paths have different safety properties (see sshArgv), and a rule that
// enumerates the dangerous characters is a rule that misses the next one.
// Quoting in sshArgv is the belt; this is the braces, and it also turns a
// remote-shell surprise into an early, readable error.
func validZFSName(s string) error {
	if s == "" {
		return fmt.Errorf("name must not be empty")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == ':' || r == '.':
		default:
			return fmt.Errorf(
				"name may use letters, digits and _-:. only (%q is not allowed)", r)
		}
	}
	return nil
}

// ─── console transport ───────────────────────────────────────────────────

// virshOut runs a read-only virsh subcommand against the current target and
// returns its stdout. Wrapping it keeps the call sites free of
// append(virsh()[1:], ...)... noise, and — the point — means no site can
// quietly hardcode qemu:///system again and work only locally.
func virshOut(args ...string) ([]byte, error) {
	argv := append(virsh(), args...)
	return exec.Command(argv[0], argv[1:]...).Output()
}
