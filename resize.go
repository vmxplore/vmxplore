// resize.go — grow a VM's disk, and the two layers inside it that a host-side
// resize leaves behind.
//
// A disk resize is three layers, and the hypervisor only owns the first:
//
//  1. the virtual disk   zfs set volsize / qemu-img resize + virsh blockresize
//  2. the partition      growpart, inside the guest
//  3. the filesystem     resize2fs / xfs_growfs / btrfs filesystem resize
//
// Stopping after (1) gives a VM that HAS 50G and can still only USE 40G: the
// guest sees a bigger /dev/vda while its partition table and its superblock
// both still describe the old size. So this verb does all three, and reports
// the filesystem size as the outcome — the number the operator actually meant.
//
// WHY the guest half runs over ssh and not the qemu guest agent: on Fedora and
// RHEL guests the agent's children run as system_u:system_r:virt_qemu_ga_t,
// which SELinux does not allow to open a raw block device. growpart there fails
// with "Permission denied" on /dev/vda while running as root with a full
// capability set, and no amount of retrying changes that. The same guest
// reached over ssh lands in unconfined_t and growpart succeeds. Measured on a
// Fedora 44 cloud guest 2026-09-01: identical script, agent path failed, ssh
// path grew 50G -> 60G. Debian's agent is not confined and works either way —
// which is precisely why testing on one distro would have shipped this broken.
//
// Growing only, never shrinking. A mounted filesystem cannot be shrunk, and
// shrinking the block device under a filesystem that still believes the old
// size destroys it. planResizeDisk refuses, and says so.
//
// The guest must be RUNNING: layers 2 and 3 need a live kernel to talk to, and
// an offline resize leaves the VM in exactly the half-done state this file
// exists to prevent.
package main

import (
	"fmt"
	"strconv"
	"strings"
)

// gib because the estate speaks whole GiB. Anything finer is a false promise —
// zfs rounds volsize up to the volblocksize regardless.
const gib = 1024 * 1024 * 1024

// growRootScript runs INSIDE the guest. Deliberately POSIX sh with no
// here-documents and no backticks: it travels as one shell-quoted argument
// through ssh, and on Fedora it cannot be written to disk first — the agent
// sandbox shows /usr read-only and /tmp noexec — so it is piped to sh and
// never lands as a file.
const growRootScript = `#!/bin/sh
# vmx-growroot — grow the root partition and filesystem to fill its disk.
#
# Runs INSIDE a guest, after the hypervisor has already grown the virtual disk
# (zfs set volsize / qemu-img resize + virsh blockresize). Growing the block
# device is only the first of three layers: the partition table and the
# filesystem both still describe the old size, so a 40G -> 50G resize leaves a
# VM that HAS 50G and can only USE 40G.
#
# What it does, in order:
#   1. find the root source device and filesystem type
#   2. derive the parent disk and partition number from it
#   3. grow the partition to the end of the disk, if it is not already there
#   4. grow the filesystem to fill the partition, by filesystem type
#   5. print before/after and exit non-zero if the size did not actually change
#
# WHY it is driven by the filesystem rather than by the distro: the command that
# grows a filesystem is a property of the FILESYSTEM (resize2fs / xfs_growfs /
# btrfs filesystem resize), not of the package manager, and the same distro
# ships different roots across images. Fedora 44 Cloud is btrfs on the last
# partition; Debian genericcloud is ext4 on p1 with p14/p15 ahead of it. The
# OS is still detected, but only to name the package an operator must install
# when a tool is missing -- never to decide which command to run.
#
# Idempotent on purpose: a guest that already grew itself (some images do)
# reports "already at full size" and exits 0. Re-running is how an operator
# recovers, so a second run must not be an error.
#
# Exit: 0 grown or already full   1 could not grow   2 unsupported layout
set -u

say() { printf '%s\n' "$*"; }
die() { printf 'vmx-growroot: %s\n' "$*" >&2; exit "${2:-1}"; }

# ── 1. what is root, and what filesystem is it ──────────────────────────────
# findmnt renders a btrfs subvolume as "/dev/vda3[/root]"; the brackets are not
# part of the device path and every later command chokes on them.
SRC="$(findmnt -no SOURCE / 2>/dev/null | sed 's/\[.*//')"
FST="$(findmnt -no FSTYPE / 2>/dev/null)"
[ -n "$SRC" ] && [ -n "$FST" ] || die "cannot determine the root device or filesystem" 2
case "$SRC" in
    /dev/*) ;;
    *) die "root is $SRC, which is not a block device (overlay/nfs root is out of scope)" 2 ;;
esac

# ── 2. parent disk + partition number ───────────────────────────────────────
# lsblk knows the parent; parsing the name is a fallback for older lsblk that
# does not fill PKNAME for every layout.
DISK="$(lsblk -no PKNAME "$SRC" 2>/dev/null | head -1)"
[ -n "$DISK" ] && DISK="/dev/$DISK"
if [ -z "$DISK" ]; then
    case "$SRC" in
        # nvme0n1p3 -> nvme0n1 ; vda3 -> vda
        *[0-9]p[0-9]*) DISK="$(printf '%s' "$SRC" | sed 's/p[0-9]*$//')" ;;
        *) DISK="$(printf '%s' "$SRC" | sed 's/[0-9]*$//')" ;;
    esac
fi
PART="$(printf '%s' "$SRC" | sed 's/.*[^0-9]\([0-9]*\)$/\1/')"
[ -n "$PART" ] || die "cannot derive a partition number from $SRC" 2
[ -b "$DISK" ] || die "derived disk $DISK is not a block device" 2

before_fs="$(df -B1 --output=size / 2>/dev/null | tail -1 | tr -d ' ')"
say "root:       $SRC ($FST) on $DISK partition $PART"
say "filesystem: $(df -h --output=size / 2>/dev/null | tail -1 | tr -d ' ') before"

# ── 3. grow the partition ───────────────────────────────────────────────────
# growpart is the right tool and is idempotent -- it exits 1 with "NOCHANGE"
# when the partition already reaches the end of the disk, which is a SUCCESS
# for us. Distinguish that from a real failure by message, not exit code.
PART_FULL=0
if command -v growpart >/dev/null 2>&1; then
    gp_out="$(growpart "$DISK" "$PART" 2>&1)"; gp_rc=$?
    case "$gp_out" in
        *NOCHANGE*|*"could only be grown by"*)
            PART_FULL=1
            say "partition:  already at full size" ;;
        *)
            if [ "$gp_rc" -eq 0 ]; then
                say "partition:  grown -- $gp_out"
                # Make the kernel re-read the new size before resizing the fs.
                partx -u "$DISK" >/dev/null 2>&1 || partprobe "$DISK" >/dev/null 2>&1 || true
            else
                say "partition:  growpart failed (rc=$gp_rc): $gp_out"
                say "            continuing -- the filesystem step below reports the truth"
            fi
            ;;
    esac
else
    # Name the package for THIS os family. This is the only place the OS
    # matters, and it only affects the advice, never the commands above.
    . /etc/os-release 2>/dev/null || true
    case "${ID_LIKE:-${ID:-}}" in
        *debian*|debian|ubuntu) pkg="cloud-guest-utils" ;;
        *rhel*|*fedora*|fedora|rhel|centos|rocky) pkg="cloud-utils-growpart" ;;
        *suse*) pkg="growpart" ;;
        arch) pkg="cloud-guest-utils" ;;
        *) pkg="cloud-utils-growpart or cloud-guest-utils" ;;
    esac
    say "partition:  growpart NOT INSTALLED on ${PRETTY_NAME:-this guest} -- install $pkg"
fi

# ── 4. grow the filesystem, by TYPE ─────────────────────────────────────────
case "$FST" in
    ext2|ext3|ext4) resize2fs "$SRC" 2>&1 | sed 's/^/            /' ;;
    xfs)            xfs_growfs / 2>&1 | sed 's/^/            /' ;;
    btrfs)          btrfs filesystem resize max / 2>&1 | sed 's/^/            /' ;;
    f2fs)           resize.f2fs "$SRC" 2>&1 | sed 's/^/            /' ;;
    *)              die "filesystem $FST is not supported by this tool" 2 ;;
esac

# ── 5. outcome, not exit code ───────────────────────────────────────────────
after_fs="$(df -B1 --output=size / 2>/dev/null | tail -1 | tr -d ' ')"
say "filesystem: $(df -h --output=size / 2>/dev/null | tail -1 | tr -d ' ') after"
if [ "${after_fs:-0}" -gt "${before_fs:-0}" ]; then
    say "RESULT: grown"
    exit 0
fi
# Nothing changed. That is correct only when there was nothing left to take,
# and the honest way to ask is where the root partition ENDS versus where the
# disk ends -- NOT disk size minus partition size. Debian's genericcloud layout
# puts p14 (BIOS boot) and p15 (ESP) BEFORE p1, so the size subtraction counts
# 128MiB of other people's partitions as free space and reports a perfectly
# grown filesystem as a failure on every second run.
# (Caught on a Debian test guest, 2026-09-01: "NOT grown -- 128MiB is still
#  unused" immediately after a successful 40G -> 50G growth.)
if [ "$PART_FULL" -eq 1 ]; then
    say "RESULT: already at full size"
    exit 0
fi
part_end=0
if command -v partx >/dev/null 2>&1; then
    part_end="$(partx -g -o END -n "$PART" "$DISK" 2>/dev/null | tr -d ' ')"
fi
disk_end="$(( $(blockdev --getsz "$DISK" 2>/dev/null || echo 0) - 1 ))"
if [ -n "$part_end" ] && [ "$part_end" -gt 0 ] 2>/dev/null; then
    # 34 sectors of GPT backup live at the end and are not usable.
    slack="$(( disk_end - part_end - 34 ))"
    if [ "$slack" -lt 32768 ]; then
        say "RESULT: already at full size"
        exit 0
    fi
    say "RESULT: NOT grown -- $((slack / 2048))MiB is still unused after $SRC"
    exit 1
fi
say "RESULT: NOT grown, and the free space after $SRC could not be measured"
exit 1
`

// guestShellArgv builds the argv that runs one shell command inside a RUNNING
// guest, as root over ssh.
//
// Two levels of quoting when the hypervisor is remote, and both are load
// bearing: sshArgv quotes the inner ssh invocation so the hypervisor's login
// shell re-parses it back into the argv we meant, and the inner call quotes the
// script so the GUEST's shell does the same. See the note on sshArgv in
// remote.go for why quoting here is a safety property rather than a formality —
// a guest IP is data, and data never becomes shell syntax.
func guestShellArgv(ip, script string) []string {
	inner := append([]string{"sudo", "-n", "ssh"}, guestSSHFlags...)
	inner = append(inner, "root@"+ip, shellQuoteArgv("/bin/sh", "-c", script))
	if target.SSHHost == "" {
		return inner
	}
	return sshArgv(target.SSHHost, inner...)
}

// guestSSHFlags is the ssh policy for reaching a GUEST, which is a different
// problem from reaching a hypervisor and needs different answers.
//
// Host keys: a guest here is a clone off a golden. It is created, destroyed and
// recreated constantly, and libvirt hands out the same 192.168.122.x address to
// whatever asks next -- fiend's root known_hosts carried three different keys
// for each of five addresses on 2026-09-02. So a CHANGED host key is the normal
// case, not an attack, and `accept-new` (which only auto-accepts UNKNOWN hosts)
// fails hard on exactly that. This is the narrow situation the project rule
// allows StrictHostKeyChecking=no for, and it is scoped to guest connections
// only: sshFlags, used for the durable hypervisor connection, stays strict.
// UserKnownHostsFile=/dev/null keeps the churn out of the file entirely rather
// than growing an ever-staler list of dead clones.
var guestSSHFlags = []string{
	"-o", "BatchMode=yes",
	"-o", "StrictHostKeyChecking=no",
	"-o", "UserKnownHostsFile=/dev/null",
	"-o", "GlobalKnownHostsFile=/dev/null",
	"-o", "LogLevel=ERROR",
	"-o", "ConnectTimeout=10",
}

// resizeTarget picks the guest-side disk name (vda) for the row's root disk.
// Returns "" when the row has no disk this verb can act on.
func resizeTarget(r Row) string {
	for _, d := range r.D.Disks {
		if d.Dev != "" && d.Dev == r.Backing {
			return d.Target
		}
		if d.File != "" && d.File == r.Backing {
			return d.Target
		}
	}
	// Fall back to the first disk: a row whose Backing came from the dataset
	// join rather than the domain XML still has exactly one root disk.
	for _, d := range r.D.Disks {
		if d.Target != "" {
			return d.Target
		}
	}
	return ""
}

// planResizeDisk grows a VM's disk to newGiB and then grows the partition and
// filesystem inside it, so the guest can actually use the space.
//
// curBytes is the disk's current virtual size, passed in rather than read here
// so that command construction stays pure and testable (resize_test.go), which
// is the same split every other verb in this package uses.
//
// Refuses, with a reason the operator can act on: a shrink, a stopped guest, a
// guest with no address, and a row with no disk.
func planResizeDisk(r Row, newGiB int, curBytes uint64) (verbPlan, error) {
	if r.Synthetic {
		return verbPlan{}, fmt.Errorf("no domain behind this row")
	}
	if newGiB <= 0 {
		return verbPlan{}, fmt.Errorf("new size must be a positive number of GiB")
	}
	newBytes := uint64(newGiB) * gib

	// Fail CLOSED on an unknown current size. Without it the shrink guard below
	// silently does not apply, and the one input this verb must never get wrong
	// is the direction of the change.
	if curBytes == 0 {
		return verbPlan{}, fmt.Errorf(
			"cannot determine the current size of %s — refusing to resize a disk of unknown size", r.D.Name)
	}
	// One way only. A mounted filesystem cannot shrink, and shrinking the block
	// device under a filesystem that still believes the old size destroys it.
	// There is no "are you sure" for this: it is refused.
	if newBytes < curBytes {
		return verbPlan{}, fmt.Errorf(
			"disk is %s and cannot shrink to %dG — growing is one-way, and shrinking a live filesystem destroys it",
			humanGiB(curBytes), newGiB)
	}
	if newBytes == curBytes {
		return verbPlan{}, fmt.Errorf("disk is already %s", humanGiB(curBytes))
	}
	if r.Backing == "" {
		return verbPlan{}, fmt.Errorf("%s has no disk to resize", r.D.Name)
	}
	tgt := resizeTarget(r)
	if tgt == "" {
		return verbPlan{}, fmt.Errorf("cannot tell which guest disk backs %s", r.D.Name)
	}
	// Layers 2 and 3 need a live kernel. Growing only the block device is the
	// half-done state this verb exists to avoid, so refuse rather than do half.
	if !r.D.Active {
		return verbPlan{}, fmt.Errorf(
			"%s is %s — start it first: the partition and filesystem can only be grown in a running guest",
			r.D.Name, r.D.State)
	}
	ip := firstGuestIP(r)
	if ip == "" {
		return verbPlan{}, fmt.Errorf(
			"%s has no reachable address yet — the guest half of the resize runs over ssh", r.D.Name)
	}

	size := strconv.Itoa(newGiB) + "G"
	var cmds [][]string
	if r.DS != nil && r.DS.Type == "volume" {
		cmds = append(cmds, zfsArgv("set", "volsize="+size, r.DS.Name))
	} else {
		// File-backed. qemu-img runs on the hypervisor, same routing as zfs.
		q := []string{"qemu-img", "resize", r.Backing, size}
		if target.SSHHost != "" {
			q = sshArgv(target.SSHHost, q...)
		}
		cmds = append(cmds, q)
	}
	// Tell the running guest's virtio device it grew. Without this the guest
	// kernel keeps the old capacity until it is rebooted.
	cmds = append(cmds, virsh("blockresize", r.D.Name, tgt, size))
	// Then the two layers inside.
	cmds = append(cmds, guestShellArgv(ip, growRootScript))

	return verbPlan{
		title: fmt.Sprintf("grow %s to %s (disk, partition and filesystem)", r.D.Name, size),
		warn: "growing is one-way: the disk cannot be shrunk back. " +
			"The guest's root filesystem is grown in place while it runs.",
		needsRoot: r.DS != nil && r.DS.Type == "volume",
		cmds:      cmds,
	}, nil
}

// firstGuestIP returns an address the hypervisor can ssh to, or "".
// Loopback is filtered upstream in libvirt.go; this only picks the first.
func firstGuestIP(r Row) string {
	for _, ip := range r.D.IPs {
		if s := strings.TrimSpace(ip); s != "" {
			return s
		}
	}
	return ""
}

// humanGiB renders a byte count the way the resize dialog and its errors talk
// about disks: whole GiB, no false precision.
func humanGiB(b uint64) string {
	return strconv.FormatUint(b/gib, 10) + "G"
}
