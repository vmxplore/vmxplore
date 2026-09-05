// appliance_substrate.go — the shared ground every appliance recipe stands on.
//
// WHAT IT DOES, IN ORDER, inside the guest:
//   1. Fails loudly and early: set -Eeuo pipefail plus an ERR trap that names
//      the line and the command.
//   2. Detects the package family (rpm/deb) once, so a recipe says
//      `app_pkg nginx` instead of branching in twelve places.
//   3. Turns the appliance's second disk into a real ZFS pool, then hands
//      recipes `app_dataset` so the tuned properties in a recipe -- 16K
//      records for a SQLite library, 1M and primarycache=metadata for media,
//      sync=disabled plus a quota for regenerable cache -- actually apply.
//   4. Provides firewall, SELinux, service-enable and verification helpers
//      that behave the same on both families.
//
// WHY IT EXISTS: the catalog's recipes installed a package and stopped. Every
// substrate property the project sells -- per-workload dataset tuning,
// snapshots you can roll back to, a firewall zone scoped to the LAN, SELinux
// contexts that survive a relabel -- was absent, so an appliance built here
// was indistinguishable from `apt install`. Measured 2026-09-03: the Plex
// recipe was 56 lines and did exactly one substrate thing, `systemctl enable`.
//
// The guests are btrfs cloud images with no pool of their own, which is why
// step 3 exists at all: without a disk to make a pool from, every zfs line in
// a recipe silently no-ops and the tuning is decoration.
//
// Notes:
//   - app_pool_init REFUSES to touch anything that is not provably blank. It
//     is the only destructive step here and it is treated that way.
//   - Everything is idempotent. Re-running a recipe is how an operator
//     recovers, so a second run must be safe and must not re-wipe.

package main

import (
	"fmt"
	"strings"
	"time"
)

// appliancePrologue is prepended to every recipe by Appliance.Render.
//
// It is fixed text: no interpolation, no operator input reaches it. Recipes
// read their own fields as shell variables and call the helpers below.
// applianceSubstrateMarker is the first line of appliancePrologue. Tests cut
// a rendered script here to isolate the operator-supplied field assignments
// from the fixed substrate below them.
const applianceSubstrateMarker = "# ─── kldload appliance substrate ───"

const appliancePrologue = `
# ─── kldload appliance substrate ────────────────────────────────────────────
set -Eeuo pipefail
APP_TAG="${APP_TAG:-appliance}"
trap 'printf "[%s] FATAL line %s: %s\n" "$APP_TAG" "$LINENO" "$BASH_COMMAND" >&2' ERR

app_log()  { printf '[%s] %s\n' "$APP_TAG" "$*"; logger -t "$APP_TAG" -- "$*" 2>/dev/null || true; }
app_warn() { printf '[%s] WARN: %s\n' "$APP_TAG" "$*" >&2; }
app_die()  { printf '[%s] FATAL: %s\n' "$APP_TAG" "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || app_die "must run as root"

# ─── family ─────────────────────────────────────────────────────────────────
# Detected once. A recipe that needs to branch says [ "$APP_FAMILY" = rpm ],
# it does not re-probe.
if command -v dnf >/dev/null 2>&1; then
    APP_FAMILY=rpm
elif command -v apt-get >/dev/null 2>&1; then
    APP_FAMILY=deb
    export DEBIAN_FRONTEND=noninteractive
else
    app_die "no dnf or apt-get — unsupported guest"
fi
app_log "family: $APP_FAMILY  ($(. /etc/os-release; echo "$PRETTY_NAME"))"

# app_pkg <name>... — install packages, family-aware, idempotent.
#
# Deliberately NOT --skip-broken / --ignore-missing: one unavailable name
# aborting the batch is the correct failure, because the alternative is a
# half-installed appliance that reports success. A recipe that wants an
# OPTIONAL package calls app_pkg_optional, which gets its own transaction.
app_pkg() {
    [ $# -gt 0 ] || return 0
    app_log "installing: $*"
    if [ "$APP_FAMILY" = rpm ]; then
        dnf -y install "$@" >/dev/null
    else
        apt-get update -qq >/dev/null
        apt-get install -y -qq "$@" >/dev/null
    fi
    # Package databases record intent; files record fact. Check the artefact.
    local _p _missing=""
    for _p in "$@"; do
        case "$_p" in http*|*/*) continue ;; esac
        if [ "$APP_FAMILY" = rpm ]; then
            rpm -q "$_p" >/dev/null 2>&1 || _missing="$_missing $_p"
        else
            dpkg -s "$_p" >/dev/null 2>&1 || _missing="$_missing $_p"
        fi
    done
    [ -z "$_missing" ] || app_die "transaction returned 0 but these are NOT installed:$_missing"
}

# app_pkg_optional — its own transaction, so failing costs only itself.
app_pkg_optional() {
    [ $# -gt 0 ] || return 0
    if [ "$APP_FAMILY" = rpm ]; then
        dnf -y install "$@" >/dev/null 2>&1 || app_warn "optional not installed: $*"
    else
        apt-get install -y -qq "$@" >/dev/null 2>&1 || app_warn "optional not installed: $*"
    fi
}

# ─── storage ────────────────────────────────────────────────────────────────
APP_POOL="${APP_POOL:-tank}"
# EXPORTED: the checks at the end of every recipe run under bash -c, a new
# process that sees only the environment. Unexported, "$APP_POOL"/pgdata was
# ""/pgdata in there, and four datasets that were tuned exactly right were
# reported FAIL (web stack, seedbox, jellyfin ×2 — onyx, 2026-09-04).
export APP_POOL

# app_pool_init — make APP_POOL out of this appliance's blank data disk.
#
# THE ONLY DESTRUCTIVE STEP IN THIS FILE, and it is written to refuse rather
# than guess. A candidate disk must be ALL of:
#   - a whole disk, not a partition, not the one carrying /
#   - holding no partition table, no filesystem, no existing pool label
#   - not mounted, not swap, not in use by device-mapper
# Anything else and we fall back to plain directories with a warning. An
# appliance without tuned datasets still works; an appliance that ate the
# wrong disk is unrecoverable, and "zpool create" does not ask twice.
# app_install_zfs — put ZFS into a guest that shipped without it.
#
# Stock cloud images carry no ZFS, and an appliance whose whole storage story
# is dataset tuning cannot shrug that off. rpm: the OpenZFS repo release RPM
# is versioned, so candidates are tried newest-first rather than pinning a
# literal that goes stale. deb: zfs lives in contrib, which cloud images do
# not enable; the deb822 Components line gains it. Both end in a dkms build —
# minutes, not seconds; the sealed-golden path is the fast lane later.
app_install_zfs() {
    command -v zpool >/dev/null 2>&1 && return 0
    app_log "installing ZFS in the guest (dkms build — several minutes)"
    if [ "$APP_FAMILY" = rpm ]; then
        # headers for the RUNNING kernel; the repo may have moved past it, in
        # which case generic kernel-devel plus the distro kernel update is
        # still a working dkms target after reboot -- but try exact first.
        dnf -y install "kernel-devel-$(uname -r)" >/dev/null 2>&1 ||
            dnf -y install kernel-devel >/dev/null 2>&1 || true
        # kldload's proven resolver, ported verbatim in spirit: revisions
        # newest-first ACROSS the current release and the fc43 bridge --
        # zfsonlinux publishes release RPMs late for new Fedoras, while the
        # per-release package dirs (baseurl $releasever) usually exist. Probe
        # with curl BEFORE dnf so a miss is one clear line, not 200 lines of
        # metadata noise. The first live run tried 2-9..2-6 on the current
        # dist only and 404ed on all of them; the host's own installer was
        # sitting on the working loop the whole time.
        local _cur _rel _rev _url _got=""
        _cur="$(rpm -E %fedora)"
        for _rel in "$_cur" 43; do
            for _rev in 3-0 2-10 2-9 2-8; do
                _url="https://zfsonlinux.org/fedora/zfs-release-${_rev}.fc${_rel}.noarch.rpm"
                if [ "$(curl -sSL -o /dev/null --max-time 10 -w '%{http_code}' "$_url" 2>/dev/null)" = 200 ]; then
                    app_log "zfs-release found: ${_rev}.fc${_rel}"
                    _got="$_url"; break 2
                fi
            done
        done
        if [ -z "$_got" ]; then
            app_warn "no OpenZFS release RPM answered for fc${_cur} or the fc43 bridge"
            return 1
        fi
        dnf -y install "$_got" >/dev/null 2>&1 || return 1
        # The fcNN bridge ships its signing keys NAMED PER RELEASE, and the
        # repo file it installs says gpgkey=...-openzfs-fedora-$releasever.
        # On a newer Fedora that exact file does not exist, and the install
        # dies at signature check: "cannot open file ...-fedora-44" (smk-web,
        # 2026-09-04, after 232 packages downloaded clean). Same key, wrong
        # name — give dnf the shipped key under the name it insists on, which
        # keeps gpgcheck ON rather than switching it off.
        local _want="/etc/pki/rpm-gpg/RPM-GPG-KEY-openzfs-fedora-${_cur}" _have
        if [ ! -f "$_want" ]; then
            _have="$(ls -1 /etc/pki/rpm-gpg/RPM-GPG-KEY-openzfs-fedora-* 2>/dev/null | sort -V | tail -1)"
            if [ -n "$_have" ]; then
                cp "$_have" "$_want"
                app_log "gpg key bridged: $(basename "$_have") -> $(basename "$_want")"
            fi
        fi
        # Evidence, not /dev/null: the first live failure was invisible for
        # exactly that reason.
        dnf -y install zfs >/var/log/appliance-zfs-install.log 2>&1 || {
            app_warn "dnf install zfs failed — tail of /var/log/appliance-zfs-install.log:"
            tail -5 /var/log/appliance-zfs-install.log >&2
            return 1
        }
    else
        # zfs-dkms sits in contrib; cloud images enable main only.
        if [ -d /etc/apt/sources.list.d ]; then
            sed -i '/^Components:/ { /contrib/! s/$/ contrib/ }'                 /etc/apt/sources.list.d/*.sources 2>/dev/null || true
        fi
        sed -i '/^deb .* main *$/ s/$/ contrib/' /etc/apt/sources.list 2>/dev/null || true
        apt-get update -qq >/dev/null 2>&1 || true
        apt-get install -y -qq "linux-headers-$(uname -r)" >/dev/null 2>&1 ||
            apt-get install -y -qq linux-headers-amd64 >/dev/null 2>&1 || true
        apt-get install -y -qq zfsutils-linux zfs-dkms >/var/log/appliance-zfs-install.log 2>&1 || {
            app_warn "apt install zfs failed — tail of /var/log/appliance-zfs-install.log:"
            tail -5 /var/log/appliance-zfs-install.log >&2
            return 1
        }
    fi
    modprobe zfs 2>/dev/null || true
    command -v zpool >/dev/null 2>&1
}

app_pool_init() {
    if ! command -v zpool >/dev/null 2>&1; then
        if ! app_install_zfs; then
            app_warn "ZFS could not be installed — datasets will be plain directories"
            APP_POOL=""
            return 0
        fi
        app_log "ZFS installed: $(zfs version 2>/dev/null | head -1)"
    fi
    if zpool list -H -o name "$APP_POOL" >/dev/null 2>&1; then
        app_log "pool '$APP_POOL' already present"
        return 0
    fi

    local _root_src _root_disk _cand="" _d _pk
    # btrfs reports the root source as /dev/vda5[/root] — the [subvol] suffix
    # is not a block device, lsblk chokes on it, and under set -e the failing
    # pipeline killed the whole recipe. It fired the FIRST time this line ever
    # ran, because every earlier run died before ZFS was installed (smk-web,
    # 2026-09-04, minutes after the dkms chain finally succeeded).
    _root_src="$(findmnt -no SOURCE / 2>/dev/null | sed 's/\[.*$//')"
    # || true: an unparseable source must degrade to "no exclusion", not kill
    # the recipe — the blkid signature check below still refuses the root
    # disk, because a disk carrying a filesystem is never a blank candidate.
    _root_disk="$(lsblk -no PKNAME "$_root_src" 2>/dev/null | head -1 || true)"
    [ -n "$_root_disk" ] || _root_disk="$(printf '%s' "${_root_src##*/}" | sed 's/p\?[0-9]*$//')"

    while read -r _d; do
        [ -n "$_d" ] || continue
        [ "$_d" = "$_root_disk" ] && continue
        # Whole disk only, and nothing may sit on it.
        [ "$(lsblk -dno TYPE "/dev/$_d" 2>/dev/null)" = "disk" ] || continue
        [ -n "$(lsblk -no NAME "/dev/$_d" 2>/dev/null | sed 1d)" ] && continue
        [ -n "$(lsblk -no MOUNTPOINT "/dev/$_d" 2>/dev/null | tr -d ' \n')" ] && continue
        # blkid prints a signature for ANY known fs/partition-table/pool label.
        if blkid -p "/dev/$_d" >/dev/null 2>&1; then continue; fi
        _cand="$_d"
        break
    done < <(lsblk -dno NAME 2>/dev/null)

    if [ -z "$_cand" ]; then
        app_warn "no blank data disk found — datasets will be plain directories"
        app_warn "  (attach one and re-run; nothing was modified)"
        APP_POOL=""
        return 0
    fi

    # by-id where the kernel offers one: a pool built on /dev/vdb breaks the
    # first time the guest gets another disk and the names shuffle.
    _pk="/dev/$_cand"
    for _l in /dev/disk/by-id/*; do
        [ -e "$_l" ] || continue
        case "$_l" in *-part*) continue ;; esac
        [ "$(readlink -f "$_l")" = "/dev/$_cand" ] && { _pk="$_l"; break; }
    done

    app_log "creating pool '$APP_POOL' on $_pk (verified blank)"
    zpool create -o ashift=12 -O compression=zstd -O atime=off \
        -O xattr=sa -O acltype=posixacl -O mountpoint=none "$APP_POOL" "$_pk" ||
        app_die "zpool create failed on $_pk"
    zpool list -H -o name "$APP_POOL" >/dev/null 2>&1 ||
        app_die "zpool create returned 0 but '$APP_POOL' does not exist"
}

# app_dataset <child> <mountpoint> [prop=value]... — idempotent.
# Falls back to a plain directory when there is no pool, so a recipe reads the
# same either way.
app_dataset() {
    local _child="$1" _mnt="$2"; shift 2
    if [ -z "${APP_POOL:-}" ]; then
        install -d -m 0755 "$_mnt"
        return 0
    fi
    local _ds="${APP_POOL}/${_child}"
    if zfs list -H -o name "$_ds" >/dev/null 2>&1; then
        app_log "dataset $_ds exists"
    else
        local _args="" _p
        for _p in "$@"; do _args="$_args -o $_p"; done
        # shellcheck disable=SC2086  # deliberate word-split of the -o pairs
        zfs create -o mountpoint="$_mnt" $_args "$_ds" ||
            app_die "zfs create $_ds failed"
        app_log "dataset $_ds -> $_mnt ($*)"
    fi
    mountpoint -q "$_mnt" || zfs mount "$_ds" 2>/dev/null || true
    # A freshly created directory or mountpoint inherits its PARENT dir's
    # SELinux type, not the policy's path match: /var/www made here came up
    # var_t, and nginx served 403 on a file it could read by DAC (smk-web,
    # 2026-09-04). restorecon applies whatever the policy says this path
    # should be; where SELinux is absent it is a no-op.
    command -v restorecon >/dev/null 2>&1 && restorecon -RF "$_mnt" 2>/dev/null || true
}

# app_snapshot <label> — a rollback point for the state worth keeping.
app_snapshot() {
    [ -n "${APP_POOL:-}" ] || return 0
    # Declared and assigned separately: a combined "local x=$(cmd)" takes
    # local's exit status, not the command's, so a failing date would go
    # unnoticed (SC2155). NB: no backticks anywhere in this string -- it is a
    # Go raw literal and a backtick would end it mid-script.
    local _stamp _ds
    _stamp="$1-$(date +%Y%m%d%H%M%S)"
    while read -r _ds; do
        [ -n "$_ds" ] || continue
        zfs snapshot "${_ds}@${_stamp}" && app_log "snapshot ${_ds}@${_stamp}"
    done < <(zfs list -H -o name -r "$APP_POOL" 2>/dev/null | sed 1d)
}

# ─── services, firewall, SELinux ────────────────────────────────────────────

# app_enable <unit>... — install and enable are ONE operation, and the enable
# is VERIFIED. A unit present on disk and not enabled is the failure this
# project keeps rediscovering: it surfaces weeks later as "why is this empty".
app_enable() {
    local _u _ok=0
    for _u in "$@"; do
        # systemctl cat, not list-unit-files: an instance of a template
        # (vdi-session@1) is never in the unit-file list, so the VDI tile's
        # sessions were skipped here and stayed disabled while every check
        # after them failed (onyx, 2026-09-04). cat resolves the template.
        systemctl cat "${_u}.service" >/dev/null 2>&1 || continue
        systemctl enable --now "${_u}.service" >/dev/null 2>&1 || true
        if systemctl is-enabled "${_u}.service" >/dev/null 2>&1; then
            app_log "enabled: ${_u}"
            _ok=1
        else
            app_warn "${_u} is NOT enabled — it will not come back after a reboot"
        fi
    done
    return 0
}

# app_firewall <zone> <source-cidr> <port/proto>... — firewalld where present,
# nftables otherwise. Scoped to a source on purpose: an appliance that opens a
# media port to the world is a different product.
app_firewall() {
    local _zone="$1" _src="$2"; shift 2
    if command -v firewall-cmd >/dev/null 2>&1; then
        systemctl enable --now firewalld >/dev/null 2>&1 || true
        firewall-cmd --permanent --new-zone="$_zone" >/dev/null 2>&1 || true
        firewall-cmd --permanent --zone="$_zone" --add-source="$_src" >/dev/null 2>&1 || true
        # A source-bound zone captures EVERYTHING from that source, not just
        # the ports listed — including ssh from the hypervisor, which sits
        # inside the LAN CIDR. That is how jellyfin and plex rejected the
        # enrollment key push and the operator's own ssh (onyx, 2026-09-04)
        # while the nft tiles, whose chain policy is accept, never noticed.
        # ssh stays key-only; the zone just has to let it through.
        firewall-cmd --permanent --zone="$_zone" --add-service=ssh >/dev/null 2>&1 ||
            app_warn "could not allow ssh in zone $_zone — enrollment will not reach this VM"
        local _p
        for _p in "$@"; do
            firewall-cmd --permanent --zone="$_zone" --add-port="$_p" >/dev/null 2>&1 ||
                app_warn "could not open $_p"
        done
        firewall-cmd --reload >/dev/null 2>&1 || true
        app_log "firewalld zone '$_zone' source $_src ports: $*"
    elif command -v nft >/dev/null 2>&1; then
        nft list table inet "$_zone" >/dev/null 2>&1 || nft add table inet "$_zone"
        nft list chain inet "$_zone" input >/dev/null 2>&1 ||
            nft add chain inet "$_zone" input '{ type filter hook input priority 0; policy accept; }'
        local _p _port _proto
        for _p in "$@"; do
            _port="${_p%%/*}"; _proto="${_p##*/}"
            nft add rule inet "$_zone" input ip saddr "$_src" "$_proto" dport "${_port//-/-}" accept 2>/dev/null ||
                app_warn "could not add nft rule for $_p"
        done
        app_log "nftables table '$_zone' source $_src ports: $*"
    else
        app_warn "no firewalld or nft — ports left to the host firewall"
    fi
}

# app_selinux <type> <path-regex> — no-op where SELinux is not enforcing.
app_selinux() {
    selinuxenabled 2>/dev/null || return 0
    # semanage is not on the Fedora cloud image. Without it this was a silent
    # no-op and every dataset a confined service wrote to stayed unlabeled_t.
    if ! command -v semanage >/dev/null 2>&1 && command -v dnf >/dev/null 2>&1; then
        dnf -y -q install policycoreutils-python-utils >/dev/null 2>&1 ||
            app_warn "policycoreutils-python-utils did not install — cannot label $2 as $1"
    fi
    command -v semanage >/dev/null 2>&1 || return 0
    semanage fcontext -a -t "$1" "$2" 2>/dev/null ||
        semanage fcontext -m -t "$1" "$2" 2>/dev/null || true
}

app_relabel() {
    command -v restorecon >/dev/null 2>&1 || return 0
    selinuxenabled 2>/dev/null || return 0
    restorecon -RF "$@" 2>/dev/null || true
}

# ─── verification ───────────────────────────────────────────────────────────
# A recipe ends by proving the thing it built actually answers. Counted, not
# asserted: "3 of 5 checks passed" is a result, "installed successfully" is a
# claim about a package manager's exit code.
APP_CHECKS_PASS=0
APP_CHECKS_FAIL=0

app_check() {
    local _label="$1"; shift
    printf '  %-40s ' "$_label"
    if "$@" >/dev/null 2>&1; then
        printf 'OK\n'; APP_CHECKS_PASS=$((APP_CHECKS_PASS + 1))
    else
        printf 'FAIL\n'; APP_CHECKS_FAIL=$((APP_CHECKS_FAIL + 1))
    fi
}

# app_wait_http <url> [seconds] — services take time; failing at t=0 is noise.
app_wait_http() {
    local _url="$1" _secs="${2:-90}" _i=0
    while [ "$_i" -lt "$_secs" ]; do
        curl -fsS --max-time 2 "$_url" >/dev/null 2>&1 && return 0
        sleep 2; _i=$((_i + 2))
    done
    return 1
}

app_summary() {
    local _total=$((APP_CHECKS_PASS + APP_CHECKS_FAIL))
    printf '\n[%s] %d/%d checks passed\n' "$APP_TAG" "$APP_CHECKS_PASS" "$_total"
    if [ "$APP_CHECKS_FAIL" -gt 0 ]; then
        printf '[%s] RESULT: INCOMPLETE — %d check(s) failed\n' "$APP_TAG" "$APP_CHECKS_FAIL"
        return 1
    fi
    printf '[%s] RESULT: VERIFIED\n' "$APP_TAG"
    return 0
}
# ─── end substrate ──────────────────────────────────────────────────────────
`

// ─── substrate requirements ─────────────────────────────────────────────────
//
// Recipes are written for kldload — KVM with ZFS underneath — and that is the
// default every tile assumes. But vmxplore also talks to plain libvirt hosts
// with no pool, and a recipe whose whole value is `recordsize=16K` for the
// library and `1M` for media has to say so rather than quietly degrade.
//
// So a recipe declares what it NEEDS, the host is probed once, and the picker
// says which of the three it is: recommended, degraded, or unavailable. A
// media server still runs without a pool; it just loses the tuning, the cache
// quota and the rollback, so "degraded" is honest where "unavailable" would
// be a lie and silence would be worse than both.

type Substrate int

const (
	// NeedsKVM — any libvirt host. The recipe uses no host storage features.
	NeedsKVM Substrate = iota
	// NeedsZFS — the recipe's datasets, quotas or snapshots are the point.
	// Runs without a pool, with its storage story reduced to plain
	// directories, which is what "degraded" tells the operator.
	NeedsZFS
)

// HostCaps is what the target actually offers. Probed, never assumed.
type HostCaps struct {
	KVM bool
	ZFS bool // a usable zpool on the host, so an appliance data disk is a zvol
}

// Availability is the picker's verdict for one recipe on one host.
type Availability struct {
	Level  string // "recommended" | "degraded" | "unavailable"
	Reason string // shown under the tile; empty when recommended
}

// Availability reports how well this recipe fits the host in front of it.
func (a Appliance) Availability(h HostCaps) Availability {
	if !h.KVM {
		return Availability{"unavailable", "no KVM on this host"}
	}
	if a.Needs == NeedsZFS && !h.ZFS {
		// Since the substrate learned to install ZFS inside the guest, a
		// pool-less HOST no longer costs the recipe its datasets — the guest
		// builds its own pool on the data disk either way. What a bare KVM
		// host loses is the host-side story: qcow2 instead of zvols (no
		// instant clones, no whole-VM snapshots) and no estate to enroll in.
		return Availability{"degraded",
			"no ZFS on the host — the appliance and its tuned datasets still " +
				"build (the guest carries its own pool), but the VM sits on " +
				"qcow2: no instant clones, no whole-VM snapshots, no estate " +
				"enrollment"}
	}
	return Availability{"recommended", ""}
}

// checkPoolName validates a ZFS pool name before it can reach `zpool create`.
//
// This is operator input on its way to a command that formats a disk, so the
// rule is allow-list, not deny-list. OpenZFS itself permits more than this
// (colons, dots, some unicode); we take the conservative subset because a
// name that round-trips through shell, systemd unit text and a mountpoint has
// more places to go wrong than a pool needs.
//
// Also rejects the reserved prefixes zfs refuses later anyway — better to say
// so in the form than to fail half way through building the appliance.
func checkPoolName(s string) error {
	if s == "" {
		return fmt.Errorf("pool name is required")
	}
	if len(s) > 64 {
		return fmt.Errorf("pool name is too long (max 64)")
	}
	if !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return fmt.Errorf("pool name must start with a letter: %q", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return fmt.Errorf("pool name may only contain letters, digits, '-' and '_': %q", s)
		}
	}
	for _, bad := range []string{"mirror", "raidz", "draid", "spare", "log", "cache"} {
		if strings.HasPrefix(strings.ToLower(s), bad) {
			return fmt.Errorf("%q starts with the reserved vdev word %q — zpool will refuse it", s, bad)
		}
	}
	return nil
}

// ─── live host probe for the picker ─────────────────────────────────────────

var hostCapsCache struct {
	at   time.Time
	caps HostCaps
}

// CurrentHostCaps probes what the target host offers RIGHT NOW, cached for
// 30s — the picker repaints every refresh and libvirt does not need the
// traffic. A pool imported mid-session shows up within the half-minute.
func CurrentHostCaps() HostCaps {
	if time.Since(hostCapsCache.at) < 30*time.Second && !hostCapsCache.at.IsZero() {
		return hostCapsCache.caps
	}
	kvm := false
	if lv, err := ConnectSystem(); err == nil {
		kvm = true
		lv.Close()
	}
	hostCapsCache.caps = HostCaps{KVM: kvm, ZFS: HasZFS()}
	hostCapsCache.at = time.Now()
	return hostCapsCache.caps
}

// ApplianceFit is the one-line answer to "what happens if I click this HERE".
// Level drives the colour; the blurb is the detail text beside the tile.
func ApplianceFit(a Appliance) (level, blurb string) {
	caps := CurrentHostCaps()
	av := a.Availability(caps)
	switch av.Level {
	case "unavailable":
		return av.Level, "unavailable here — " + av.Reason
	case "degraded":
		return av.Level, "degraded here — " + av.Reason
	}
	// recommended, with the estate story told rather than implied
	if KldloadTier() == "kldload" {
		return "full", "full build here · estate enrollment: mesh + CA + inventory"
	}
	if caps.ZFS {
		return "full", "full build here · zvol-backed (no estate enrollment: host is not kldload)"
	}
	return "full", ""
}
