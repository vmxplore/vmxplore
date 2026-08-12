//go:build gui

// gui_only.go — helpers reached only from the GUI.
//
// These live behind the tag because the static TUI never calls them, and an
// untagged file made staticcheck report them as dead code in the static
// build — correctly. Silencing that would have hidden a real signal; moving
// them keeps the signal and the code.
//
//	nvidiaGuestScript  the in-guest driver installer New VM offers
//	reexec             the Connect button relaunching against another host
//	vncEndpoint        the console's dial address, tunnelled when remote
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// nvidiaGuestScript makes a Debian guest ready to drive an NVIDIA card.
// It is a LAYER: composed onto whatever post-install the operator or an
// appliance already supplies, exactly as the writing desktop is composed
// onto the WriteFreely server script.
//
// The order matters and each step earned its place:
//
//  1. The cloud image ships "Components: main" only — verified on a live
//     Debian 13 guest, where nvidia-driver has no installation candidate
//     at all until contrib and non-free are enabled.
//  2. The driver is a kernel module built by DKMS, so it needs headers
//     that match the RUNNING kernel. The cloud kernel flavour has no DRM
//     and is the wrong host for a GPU driver, so the generic kernel and
//     its headers go on together.
//  3. grub prefers the cloud kernel (same version, and its name sorts
//     later), so the generic entry is named explicitly or the machine
//     reboots straight back into a kernel the driver cannot load into.
//     This is the same trap the writing desktop hit; see appliances.go.
//  4. One reboot, then nvidia-smi is the proof. Its output lands in the
//     cloud-init log and on both consoles.
//
// WARN: this installs from Debian's non-free component. Nothing is
// redistributed here — the guest fetches it from Debian — but the
// operator is accepting NVIDIA's licence, and a catalog entry that uses
// this layer should say so in its notes.
//
// WARN: DKMS rebuilds on every kernel update, which is where a VM comes
// back without its GPU after an unattended upgrade. Seal a golden and
// clone from it rather than running this per machine.
const nvidiaGuestScript = `
# ─── NVIDIA drivers ──────────────────────────────────────────────────
if ! command -v apt-get >/dev/null 2>&1; then
    echo "WARN: NVIDIA layer supports Debian/Ubuntu guests only — skipping" >&2
else
    export DEBIAN_FRONTEND=noninteractive

    # deb822 sources on trixie: widen Components in place. The older
    # one-line format is handled too, since an Ubuntu image may use it.
    for src in /etc/apt/sources.list.d/*.sources; do
        [ -e "$src" ] || continue
        sed -i 's/^Components: main$/Components: main contrib non-free non-free-firmware/' "$src"
    done
    if [ -f /etc/apt/sources.list ]; then
        sed -i 's/^\(deb .*main\)$/\1 contrib non-free non-free-firmware/' \
            /etc/apt/sources.list
    fi
    apt-get update

    nv_arch="$(dpkg --print-architecture)"
    apt-get install -y "linux-image-$nv_arch" "linux-headers-$nv_arch"
    apt-get install -y nvidia-driver firmware-misc-nonfree

    # Boot the kernel the driver was built for. Both flavours carry the
    # same version and grub's sort prefers the cloud one, so the generic
    # entry is named outright, by ids read from the generated grub.cfg.
    nv_gen=""
    for nv_k in /boot/vmlinuz-*; do
        case "$nv_k" in *-cloud-*) continue ;; esac
        nv_gen="$nv_k"
    done
    nv_ver="${nv_gen#/boot/vmlinuz-}"
    nv_sub="$(grep -o 'gnulinux-advanced-[a-f0-9-]*' /boot/grub/grub.cfg | head -1)"
    nv_ent="$(grep -o "gnulinux-$nv_ver-advanced-[a-f0-9-]*" /boot/grub/grub.cfg | head -1)"
    if [ -n "$nv_ver" ] && [ -n "$nv_sub" ] && [ -n "$nv_ent" ]; then
        sed -i '/^GRUB_DEFAULT=/d' /etc/default/grub
        echo "GRUB_DEFAULT=\"$nv_sub>$nv_ent\"" >>/etc/default/grub
        apt-get purge -y "linux-image-cloud-$nv_arch" ||
            echo "WARN: could not drop the cloud kernel meta package" >&2
        update-grub
    else
        echo "WARN: could not pin grub to the generic kernel — the driver" >&2
        echo "      will not load until the right kernel is booted" >&2
    fi

    echo "NVIDIA drivers installed — rebooting to load them"
    echo "(after the reboot, 'nvidia-smi' proves it; with no card passed"
    echo " through to this VM it will correctly report no devices)"
    systemctl reboot
fi
`

// reexec replaces the current process with argv[0] argv[1:], inheriting the
// environment — used by the GUI's Connect button to relaunch pointed at a
// different host (a live reconnect would have to rebuild every pane).
func reexec(exe string, argv []string) error {
	return syscall.Exec(exe, argv, os.Environ())
}

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

	// -N: set up the forward and run no remote command. The shared sshFlags
	// carry BatchMode (never sit at a password prompt with no terminal to type
	// into), the host-key policy and the connect timeout — one policy for every
	// ssh this tool starts, see remote.go. ExitOnForwardFailure is specific to
	// a forward: without it ssh stays up having bound nothing and the dial
	// below waits out its timeout for a tunnel that will never exist.
	sshOpts := append([]string{"-N"}, sshFlags...)
	sshOpts = append(sshOpts,
		"-o", "ExitOnForwardFailure=yes",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", local, port),
		target.SSHHost)
	cmd := exec.Command("ssh", sshOpts...)
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
