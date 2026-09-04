// =============================================================================
// homelab.go — the home-lab presets: the services people actually self-host.
//
// WHAT IT DOES, IN ORDER:
//   1. Adds Appliance entries for the services a home lab is usually built
//      out of — media (Jellyfin, Plex), DNS filtering (AdGuard Home), git
//      (Gitea) and file sync (Syncthing).
//   2. Each one is the same shape as every other appliance: a cloud-image
//      preset, sizing, operator fields, and a fixed bash post-installer. They
//      plug into the existing pipeline untouched.
//
// WHY IT EXISTS:
//   The catalog proved the mechanism with WriteFreely; this is the mechanism
//   pointed at the actual reason people build a lab. Every one of these has a
//   "how to self-host X" blog post behind it, and every one of those posts is
//   the same four moves — trust a repo key, install, write a unit, open a
//   port. Encoding them once turns an evening into a button, and Make Golden →
//   Clone turns the result into a template you can stamp out.
//
// WHY THE APT ENTRIES PIN A FINGERPRINT:
//   Upstream's own instructions are, without exception, `curl … | gpg --dearmor`
//   — whatever the key server hands back is trusted forever after. These
//   scripts fetch the same key and then refuse to continue unless its primary
//   fingerprint matches the constant recorded here (verified against upstream
//   on 2026-08-15). A hijacked mirror or an intercepted fetch then fails
//   loudly at install time instead of silently installing signed-by-someone-
//   else packages for the life of the VM.
//
// Notes:
//   - Each script repeats the key-verification block rather than sharing a
//     helper. That is deliberate: appliances.go promises a rendered script is
//     a standalone bash installer with no vmxplore dependency, so it cannot
//     call into anything that does not exist in the guest.
//   - Rendered scripts run as root from cloud-init runcmd under
//     `set -Eeuo pipefail` (newvm.go) — no shebang, no set line, and an
//     unchecked failure aborts the install rather than producing a half-built
//     appliance that looks ready.
//   - AdGuard Home and Syncthing finish at their own first-run wizard. Their
//     admin credentials are stored as hashes their own tooling generates, and
//     inventing that in bash is how you ship an appliance nobody can log into.
//     The Notes on those two say so plainly instead.
// =============================================================================

package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ─── Media: Jellyfin ─────────────────────────────────────────────────
//
// Debian's own archive carries no Jellyfin, so this is upstream's repo,
// pinned to the trixie suite that matches the Debian 13 cloud image. Sizing
// leans on RAM rather than CPU because transcoding is the one thing that
// will hurt, and the disk default is only enough for the library database —
// see Notes for where the media itself is meant to live.

var jellyfin = Appliance{
	Name:     "Jellyfin on ZFS",
	Summary:  "Free-software media server on tuned datasets — per-title media, 16K library, throwaway cache",
	Homepage: "https://jellyfin.org",
	License:  "GPL-2.0",

	Needs:  NeedsZFS,
	DataGB: 200,

	Distro: "fedora",
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 20,

	Port:    8096,
	LandsOn: "http://<vm-ip>:8096/  (setup wizard on first visit, or headless if admin fields set)",

	Notes: "Same dataset layout as the Plex tile, so the two media servers " +
		"are interchangeable over one library model:\n\n" +
		"  media/movies, media/tv   one dataset PER TITLE (add-movie, add-show)\n" +
		"  media/music              lz4    media/photos  compression=off\n" +
		"  jellyfin data            recordsize=16K — the library is SQLite\n" +
		"  jellyfin cache           sync=disabled, 100G quota, never snapshotted\n\n" +
		"On Fedora the package comes from RPM Fusion (upstream dropped its own " +
		"RPMs at 10.9); on Debian from Jellyfin's repo, its signing key " +
		"checked against a pinned fingerprint first.\n\n" +
		"Set the admin fields and the setup wizard runs HEADLESSLY — the VM " +
		"comes up already past first-run. Leave them empty for the browser " +
		"wizard. The password is carried in the cloud-init seed; treat it as " +
		"a bootstrap credential and rotate it in the UI.\n\n" +
		"Hardware transcoding is wired up when the VM has /dev/dri (GPU " +
		"passthrough); otherwise transcoding is CPU-only and direct play is " +
		"unaffected.",

	Fields: []ApplianceField{
		{Key: "JF_POOL", Label: "pool name",
			Placeholder: "created on the appliance's data disk",
			Default:     "tank", Required: true},
		{Key: "JF_MEDIA_DIR", Label: "library mountpoint",
			Placeholder: "where the media datasets mount",
			Default:     "/srv/media", Required: true},
		{Key: "JF_ALLOW_CIDR", Label: "allowed source",
			Placeholder: "who may reach :8096",
			Default:     "192.168.0.0/16", Required: true},
		{Key: "JF_ADMIN_USER", Label: "admin user (optional)",
			Placeholder: "set with password for a headless setup"},
		{Key: "JF_ADMIN_PASS", Label: "admin password (optional)",
			Placeholder: "required when admin user is set"},
	},

	Validate: func(v map[string]string) error {
		if err := checkAbsDir(v["JF_MEDIA_DIR"], "library mountpoint"); err != nil {
			return err
		}
		if v["JF_ADMIN_USER"] != "" && v["JF_ADMIN_PASS"] == "" {
			return fmt.Errorf("admin user is set but the password is empty")
		}
		return checkPoolName(v["JF_POOL"])
	},

	Script: jellyfinScript,
}

const jellyfinScript = `
APP_TAG=jellyfin
APP_POOL="$JF_POOL"

app_pool_init

# ─── datasets ───────────────────────────────────────────────────────────────
# Library DB + metadata: SQLite under WAL, small random IO. Snapshot-worthy.
app_dataset jellyfin       /var/lib/jellyfin   recordsize=16K
# Cache + transcodes: fully regenerable. sync=disabled is safe BECAUSE it is
# regenerable, the quota stops a runaway transcode eating the pool, and
# auto-snapshot=false keeps it out of every snapshot and replication stream.
app_dataset jellyfin-cache /var/cache/jellyfin recordsize=128K compression=lz4 \
    sync=disabled quota=100G com.sun:auto-snapshot=false

# Media: same shape as the Plex tile — movies/ and tv/ are canmount=off
# containers holding one dataset per title.
app_dataset media        none                      canmount=off
app_dataset media/movies "$JF_MEDIA_DIR/movies"    canmount=off
app_dataset media/tv     "$JF_MEDIA_DIR/tv"        canmount=off
app_dataset media/music  "$JF_MEDIA_DIR/music"     compression=lz4
app_dataset media/photos "$JF_MEDIA_DIR/photos"    compression=off
mkdir -p "$JF_MEDIA_DIR"/movies "$JF_MEDIA_DIR"/tv /var/cache/jellyfin/transcodes

# ─── install ────────────────────────────────────────────────────────────────
app_pkg curl jq
if [ "$APP_FAMILY" = rpm ]; then
    # Upstream dropped its own RPMs at 10.9 and points Fedora at RPM Fusion.
    app_pkg policycoreutils-python-utils firewalld
    _fedora_ver="$(rpm -E %fedora)"
    rpm -q rpmfusion-free-release >/dev/null 2>&1 ||
        app_pkg "https://mirrors.rpmfusion.org/free/fedora/rpmfusion-free-release-${_fedora_ver}.noarch.rpm"
    rpm -q rpmfusion-nonfree-release >/dev/null 2>&1 ||
        app_pkg_optional "https://mirrors.rpmfusion.org/nonfree/fedora/rpmfusion-nonfree-release-${_fedora_ver}.noarch.rpm"
    # The NVIDIA cuda repo fights RPM Fusion over driver packages; keep it out
    # of this one transaction rather than editing the operator's repo files.
    _dnf_excl=""
    if dnf repolist --enabled 2>/dev/null | grep -qi '^cuda'; then
        app_warn "NVIDIA cuda repo detected — excluded from the jellyfin transaction"
        _dnf_excl="--disablerepo=cuda*"
    fi
    dnf -y $_dnf_excl install jellyfin >/dev/null
    rpm -q jellyfin >/dev/null 2>&1 || app_die "dnf returned 0 but jellyfin is not installed"
else
    # Debian: upstream repo, key verified against a pinned fingerprint BEFORE
    # apt is told to trust it.
    JF_KEY_URL=https://repo.jellyfin.org/jellyfin_team.gpg.key
    JF_KEY_FPR=4918AABC486CA052358D778D49023CD01DE21A7B
    app_pkg gnupg2
    _key="$(mktemp)"; trap 'rm -f "$_key"' EXIT
    curl -fsSL --retry 5 --retry-delay 3 "$JF_KEY_URL" -o "$_key"
    gpg --show-keys --with-colons "$_key" | awk -F: '$1=="fpr"{print $10}' |
        grep -qx "$JF_KEY_FPR" || app_die "jellyfin key fingerprint mismatch — refusing to import"
    app_log "jellyfin signing key verified: $JF_KEY_FPR"
    install -d -m 0755 /etc/apt/keyrings
    gpg --dearmor <"$_key" >/etc/apt/keyrings/jellyfin.gpg
    chmod 0644 /etc/apt/keyrings/jellyfin.gpg
    . /etc/os-release
    echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/${ID} ${VERSION_CODENAME} main" \
        >/etc/apt/sources.list.d/jellyfin.list
    app_pkg jellyfin
fi
getent passwd jellyfin >/dev/null || app_die "the jellyfin user was not created by the package"

# ─── ownership, ACLs, SELinux ───────────────────────────────────────────────
chown -R jellyfin:jellyfin /var/lib/jellyfin /var/cache/jellyfin "$JF_MEDIA_DIR"
chmod 0750 /var/lib/jellyfin /var/cache/jellyfin
chmod -R 0755 "$JF_MEDIA_DIR"
if command -v setfacl >/dev/null 2>&1; then
    # Default ACL so files a downloader drops in later stay readable.
    setfacl -R -m u:jellyfin:rX "$JF_MEDIA_DIR"
    setfacl -R -d -m u:jellyfin:rX "$JF_MEDIA_DIR"
fi
app_selinux mnt_t     "${JF_MEDIA_DIR}(/.*)?"
app_selinux var_lib_t "/var/lib/jellyfin(/.*)?"
app_relabel "$JF_MEDIA_DIR" /var/lib/jellyfin /var/cache/jellyfin

# ─── systemd drop-in ────────────────────────────────────────────────────────
install -d -m 0755 /etc/systemd/system/jellyfin.service.d
cat >/etc/systemd/system/jellyfin.service.d/10-kldload.conf <<CONF
[Unit]
After=zfs-mount.service network-online.target
Wants=zfs-mount.service network-online.target
RequiresMountsFor=/var/lib/jellyfin /var/cache/jellyfin ${JF_MEDIA_DIR}

[Service]
Restart=on-failure
RestartSec=5
# Server GC helps multi-stream libraries; the heap cap keeps .NET from
# treating a 2G VM as a 2G heap budget.
Environment=DOTNET_gcServer=1
Environment=DOTNET_GCHeapHardLimitPercent=50
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
ProtectKernelTunables=yes
RestrictSUIDSGID=yes
LimitNOFILE=65535
CONF
cat >/etc/tmpfiles.d/jellyfin.conf <<'CONF'
d /var/cache/jellyfin/transcodes 0750 jellyfin jellyfin 1d
CONF

# ─── hardware transcoding, when the VM actually has a GPU ───────────────────
if [ -d /dev/dri ]; then
    app_log "GPU present — wiring VA-API transcode access"
    for _g in video render; do
        getent group "$_g" >/dev/null && usermod -aG "$_g" jellyfin
    done
    cat >/etc/udev/rules.d/99-jellyfin-dri.rules <<'RULES'
KERNEL=="renderD*", GROUP="render", MODE="0660"
KERNEL=="card*",    GROUP="video",  MODE="0660"
RULES
    udevadm control --reload-rules 2>/dev/null && udevadm trigger --subsystem-match=drm 2>/dev/null
    if [ "$APP_FAMILY" = rpm ]; then
        app_pkg_optional intel-media-driver libva-utils mesa-va-drivers
    else
        app_pkg_optional intel-media-va-driver vainfo mesa-va-drivers
    fi
    # Seed encoding.xml so first boot already has VAAPI on and transcodes on
    # the cache dataset. Jellyfin fills missing elements with defaults, so a
    # partial file is safe.
    if [ ! -f /etc/jellyfin/encoding.xml ]; then
        install -d -m 0755 /etc/jellyfin
        cat >/etc/jellyfin/encoding.xml <<'ENC'
<?xml version="1.0" encoding="utf-8"?>
<EncodingOptions xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
                 xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <TranscodingTempPath>/var/cache/jellyfin/transcodes</TranscodingTempPath>
  <HardwareAccelerationType>vaapi</HardwareAccelerationType>
  <VaapiDevice>/dev/dri/renderD128</VaapiDevice>
  <EnableHardwareEncoding>true</EnableHardwareEncoding>
  <EnableTonemapping>true</EnableTonemapping>
  <EnableThrottling>true</EnableThrottling>
  <ThrottleDelaySeconds>180</ThrottleDelaySeconds>
</EncodingOptions>
ENC
        chown jellyfin:jellyfin /etc/jellyfin/encoding.xml
        chmod 0640 /etc/jellyfin/encoding.xml
    fi
fi

# ─── large libraries outgrow the inotify defaults ───────────────────────────
cat >/etc/sysctl.d/90-jellyfin.conf <<'SYSCTL'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 1024
SYSCTL
sysctl -q --system

# ─── per-title helpers, owner-aware ─────────────────────────────────────────
for _kind in movie show; do
    _sub=movies; [ "$_kind" = show ] && _sub=tv
    cat >"/usr/local/bin/add-${_kind}" <<HELPER
#!/usr/bin/env bash
# add-${_kind} "Title" [year] — one dataset per title.
set -Eeuo pipefail
TITLE="\${1:-}"; YEAR="\${2:-}"
[ -n "\$TITLE" ] || { echo 'usage: add-${_kind} "Title" [year]' >&2; exit 2; }
SLUG=\$(printf '%s%s' "\$TITLE" "\${YEAR:+-\$YEAR}" | tr '[:upper:]' '[:lower:]' | tr ' ' '-' | tr -cd 'a-z0-9-')
[ -n "\$SLUG" ] || { echo "title produced an empty slug" >&2; exit 2; }
DATASET="${APP_POOL:-tank}/media/${_sub}/\${SLUG}"
MOUNT="${JF_MEDIA_DIR}/${_sub}/\${SLUG}"
if zfs list "\$DATASET" >/dev/null 2>&1; then echo "already exists: \$DATASET"; exit 0; fi
zfs create -o mountpoint="\$MOUNT" -o compression=off -o recordsize=1M "\$DATASET"
chown jellyfin:jellyfin "\$MOUNT"; chmod 0755 "\$MOUNT"
echo "created \$DATASET -> \$MOUNT"
HELPER
    chmod 0755 "/usr/local/bin/add-${_kind}"
done

# ─── firewall, service ──────────────────────────────────────────────────────
# 8096 http, 8920 https, 1900/udp DLNA, 7359/udp client auto-discovery.
app_firewall jellyfin "$JF_ALLOW_CIDR" 8096/tcp 8920/tcp 1900/udp 7359/udp

systemctl daemon-reload
app_enable jellyfin

app_log "waiting for Jellyfin on :8096"
app_wait_http http://127.0.0.1:8096/System/Info/Public 120 ||
    app_warn "not answering yet — journalctl -u jellyfin -n 100"

# ─── headless setup wizard ──────────────────────────────────────────────────
if [ -n "${JF_ADMIN_USER:-}" ] &&
    curl -fsS --max-time 5 http://127.0.0.1:8096/System/Info/Public 2>/dev/null |
    jq -e '.StartupWizardCompleted == false' >/dev/null 2>&1; then
    app_log "running the setup wizard headlessly"
    _jf() { curl -fsS --max-time 15 -X POST "http://127.0.0.1:8096$1" \
        -H 'Content-Type: application/json' -d "$2" >/dev/null; }
    _jf /Startup/Configuration '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'
    # GET before POST: the wizard endpoint initialises server-side state on
    # the read, and posting a user first is silently ignored.
    curl -fsS --max-time 10 http://127.0.0.1:8096/Startup/User >/dev/null
    _jf /Startup/User "$(jq -nc --arg n "$JF_ADMIN_USER" --arg p "$JF_ADMIN_PASS" '{Name:$n,Password:$p}')"
    _jf /Startup/RemoteAccess '{"EnableRemoteAccess":false,"EnableAutomaticPortMapping":false}'
    _jf /Startup/Complete '{}'
    systemctl restart jellyfin.service
    app_wait_http http://127.0.0.1:8096/System/Info/Public 60 || true
    app_log "wizard complete — admin '$JF_ADMIN_USER' created"
fi

# ─── verify ─────────────────────────────────────────────────────────────────
echo
app_check "jellyfin answers on :8096"  curl -fsS --max-time 5 http://127.0.0.1:8096/System/Info/Public
app_check "jellyfin enabled"           systemctl is-enabled jellyfin
app_check "jellyfin active"            systemctl is-active jellyfin
app_check "library mountpoint"         test -d "$JF_MEDIA_DIR/movies"
app_check "add-movie installed"        test -x /usr/local/bin/add-movie
app_check "add-show installed"         test -x /usr/local/bin/add-show
if [ -n "${APP_POOL:-}" ]; then
    app_check "library recordsize 16K" bash -c '[ "$(zfs get -H -o value recordsize "$APP_POOL"/jellyfin)" = 16K ]'
    app_check "cache never snapshotted" bash -c '[ "$(zfs get -H -o value com.sun:auto-snapshot "$APP_POOL"/jellyfin-cache)" = false ]'
    app_snapshot postinstall-jellyfin
fi

cat <<EOM

  Jellyfin on ZFS

  Web UI      http://$(hostname -I 2>/dev/null | awk '{print $1}'):8096/
  Library     ${JF_MEDIA_DIR}      pool: ${APP_POOL:-<none — plain dirs>}
  Add titles  add-movie "The Matrix" 1999   /   add-show "The Wire"
  Firewall    zone 'jellyfin', source ${JF_ALLOW_CIDR}

EOM
app_summary
`

// ─── Media: Plex ─────────────────────────────────────────────────────
//
// Same shape as Jellyfin and deliberately offered alongside it rather than
// instead of it: Plex is proprietary but free to run and has the client
// coverage (TVs, consoles) that decides the question for most people. Which
// one belongs in a given lab is not a call this catalog should make.

var plex = Appliance{
	Name:     "Plex on ZFS",
	Summary:  "Plex media server on tuned ZFS datasets — per-title datasets, 8K-record library, throwaway transcodes",
	Homepage: "https://www.plex.tv",
	License:  "Proprietary (free tier; Plex Pass optional)",

	// kldload is the substrate this is written for. Without a pool the
	// dataset layout below — which is the entire point of "Plex on ZFS" —
	// collapses to plain directories, and the picker says so.
	Needs:  NeedsZFS,
	DataGB: 200,

	Distro: "fedora",
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 20,

	Port:    32400,
	LandsOn: "http://<vm-ip>:32400/web  (claim it from a browser on this LAN)",

	Notes: "Builds the dataset layout from the Plex on ZFS recipe rather than " +
		"dropping everything in one directory:\n\n" +
		"  media/movies, media/tv   containers — each TITLE gets its own\n" +
		"                           dataset, compression=off recordsize=1M\n" +
		"  media/music              lz4 — music compresses, video does not\n" +
		"  media/photos             compression=off\n" +
		"  plex/config              recordsize=8K — the library is SQLite\n" +
		"  plex/transcode           auto-snapshot=false — regenerable\n\n" +
		"The per-title model earns itself the first time you want to move, " +
		"snapshot, send or quota one 60 GB film without touching the other " +
		"four hundred. `add-movie \"The Matrix\" 1999` is installed for that.\n\n" +
		"CLAIMING: open the web UI from a browser on the SAME subnet as the " +
		"VM. Plex treats same-subnet access as trusted; from anywhere else " +
		"you get a lock screen instead of the wizard. That is Plex, not this " +
		"install.\n\n" +
		"Proprietary: it phones home to plex.tv and needs an account. " +
		"Jellyfin is the free-software option in this catalog.",

	Fields: []ApplianceField{
		{Key: "PLEX_POOL", Label: "pool name",
			Placeholder: "created on the appliance's data disk",
			Default:     "tank", Required: true},
		{Key: "PLEX_MEDIA_DIR", Label: "library mountpoint",
			Placeholder: "where the media datasets mount",
			Default:     "/srv/media", Required: true},
		{Key: "PLEX_MOVIES_QUOTA", Label: "movies quota",
			Placeholder: "e.g. 10T, or none",
			Default:     "none"},
		{Key: "PLEX_TV_QUOTA", Label: "tv quota",
			Placeholder: "e.g. 5T, or none",
			Default:     "none"},
		{Key: "PLEX_ALLOW_CIDR", Label: "allowed source",
			Placeholder: "who may reach :32400",
			Default:     "192.168.0.0/16", Required: true},
		{Key: "PLEX_CLAIM", Label: "claim token (optional)",
			Placeholder: "claim-xxxx from plex.tv/claim — 4 minute TTL"},
	},

	Validate: func(v map[string]string) error {
		if err := checkAbsDir(v["PLEX_MEDIA_DIR"], "library mountpoint"); err != nil {
			return err
		}
		return checkPoolName(v["PLEX_POOL"])
	},

	Script: plexScript,
}

const plexScript = `
APP_TAG=plex
APP_POOL="$PLEX_POOL"

app_pool_init

# ─── datasets, per the Plex on ZFS recipe ───────────────────────────────────
# Containers first: canmount=off means "this is a namespace, not a filesystem".
# movies/ and tv/ hold one dataset PER TITLE, which is why they are containers
# rather than plain datasets with files in them.
app_dataset media          none                      canmount=off
app_dataset media/movies   "$PLEX_MEDIA_DIR/movies"  canmount=off
app_dataset media/tv       "$PLEX_MEDIA_DIR/tv"      canmount=off

# Music compresses; video does not. photos are already-compressed JPEG/HEIC.
app_dataset media/music    "$PLEX_MEDIA_DIR/music"   compression=lz4
app_dataset media/photos   "$PLEX_MEDIA_DIR/photos"  compression=off

# The library is SQLite: small random IO, so small records.
app_dataset plex           /var/lib/plexmediaserver
app_dataset plex/config    /var/lib/plexmediaserver/Library  recordsize=8K

# Transcodes are regenerable by definition — never snapshot them, and keep
# them off any replication stream.
app_dataset plex/transcode /var/tmp/plex-transcode \
    compression=off com.sun:auto-snapshot=false

if [ -n "${APP_POOL:-}" ]; then
    [ "$PLEX_MOVIES_QUOTA" = none ] || zfs set quota="$PLEX_MOVIES_QUOTA" "$APP_POOL/media/movies"
    [ "$PLEX_TV_QUOTA" = none ]     || zfs set quota="$PLEX_TV_QUOTA"     "$APP_POOL/media/tv"
fi
mkdir -p "$PLEX_MEDIA_DIR"/movies "$PLEX_MEDIA_DIR"/tv

# ─── repo + install ─────────────────────────────────────────────────────────
# The signing key is checked against a pinned fingerprint BEFORE rpm/apt is
# told to trust it. Downloading a key and importing it unverified trusts
# whoever answered the DNS query.
PLEX_KEY_URL=https://downloads.plex.tv/plex-keys/PlexSign.v2.key
PLEX_KEY_FPR=6EFFEB478A6559D75C7C4FE706C521790B9CFFDE

app_pkg curl gnupg2 jq
_key="$(mktemp)"; trap 'rm -f "$_key"' EXIT
curl -fsSL --retry 5 --retry-delay 3 "$PLEX_KEY_URL" -o "$_key"
gpg --show-keys --with-colons "$_key" | awk -F: '$1=="fpr"{print $10}' |
    grep -qx "$PLEX_KEY_FPR" || app_die "PlexSign key fingerprint mismatch — refusing to import"
app_log "plex signing key verified: $PLEX_KEY_FPR"

if [ "$APP_FAMILY" = rpm ]; then
    app_pkg policycoreutils-python-utils firewalld
    rpm --import "$_key"
    rm -f /etc/yum.repos.d/plexmediaserver.repo /etc/yum.repos.d/PlexRepo.repo
    cat >/etc/yum.repos.d/plex.repo <<'REPO'
[PlexTv]
name=Plex.tv
baseurl=https://repo.plex.tv/rpm/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://downloads.plex.tv/plex-keys/PlexSign.v2.key
skip_if_unavailable=False
REPO
    chmod 0644 /etc/yum.repos.d/plex.repo
    dnf -y makecache --refresh >/dev/null
else
    install -d -m 0755 /etc/apt/keyrings
    gpg --dearmor <"$_key" >/etc/apt/keyrings/plex.gpg
    chmod 0644 /etc/apt/keyrings/plex.gpg
    echo "deb [signed-by=/etc/apt/keyrings/plex.gpg] https://downloads.plex.tv/repo/deb public main" \
        >/etc/apt/sources.list.d/plexmediaserver.list
fi
app_pkg plexmediaserver
getent passwd plex >/dev/null || app_die "the plex user was not created by the package"

# ─── ownership, ACLs, SELinux ───────────────────────────────────────────────
chown -R plex:plex /var/lib/plexmediaserver "$PLEX_MEDIA_DIR" /var/tmp/plex-transcode
chmod 0750 /var/lib/plexmediaserver
chmod -R 0755 "$PLEX_MEDIA_DIR"
if command -v setfacl >/dev/null 2>&1; then
    # Default ACL so anything a downloader drops in later stays readable.
    setfacl -R -m u:plex:rX "$PLEX_MEDIA_DIR"
    setfacl -R -d -m u:plex:rX "$PLEX_MEDIA_DIR"
fi
app_selinux mnt_t     "${PLEX_MEDIA_DIR}(/.*)?"
app_selinux var_lib_t "/var/lib/plexmediaserver(/.*)?"
app_relabel "$PLEX_MEDIA_DIR" /var/lib/plexmediaserver

# ─── systemd drop-in ────────────────────────────────────────────────────────
# RequiresMountsFor is the load-bearing line: without it Plex can start before
# the datasets mount, see an empty library, and helpfully mark everything
# missing.
install -d -m 0755 /etc/systemd/system/plexmediaserver.service.d
cat >/etc/systemd/system/plexmediaserver.service.d/10-kldload.conf <<CONF
[Unit]
After=zfs-mount.service network-online.target
Wants=zfs-mount.service network-online.target
RequiresMountsFor=/var/lib/plexmediaserver ${PLEX_MEDIA_DIR}

[Service]
Environment=PLEX_MEDIA_SERVER_MAX_PLUGIN_PROCS=6
Environment=TMPDIR=/var/tmp/plex-transcode
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
PrivateTmp=no
ProtectSystem=full
ProtectHome=yes
ProtectKernelTunables=yes
RestrictSUIDSGID=yes
CONF

if [ -n "${PLEX_CLAIM:-}" ]; then
    cat >/etc/systemd/system/plexmediaserver.service.d/20-claim.conf <<CONF
[Service]
Environment=PLEX_CLAIM=${PLEX_CLAIM}
CONF
fi

# ─── large libraries outgrow the inotify defaults ───────────────────────────
cat >/etc/sysctl.d/90-plex.conf <<'SYSCTL'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 1024
SYSCTL
sysctl -q --system

# ─── add-movie: the per-title dataset helper ────────────────────────────────
cat >/usr/local/bin/add-movie <<'ADDMOVIE'
#!/usr/bin/env bash
# add-movie "The Matrix" 1999 — one dataset per film.
#
# A 4K rip is 50-80GB. As its own dataset it can be snapshotted, sent,
# quota'd, or moved to another pool on its own. As a file in a shared
# dataset it can only be copied.
set -Eeuo pipefail
TITLE="${1:-}"; YEAR="${2:-}"
[ -n "$TITLE" ] && [ -n "$YEAR" ] || { echo 'usage: add-movie "Movie Title" YEAR' >&2; exit 2; }
SLUG=$(printf '%s-%s' "$TITLE" "$YEAR" | tr '[:upper:]' '[:lower:]' | tr ' ' '-' | tr -cd 'a-z0-9-')
[ -n "$SLUG" ] || { echo "title produced an empty slug" >&2; exit 2; }
DATASET="__POOL__/media/movies/${SLUG}"
MOUNT="__MEDIA__/movies/${SLUG}"
if zfs list "$DATASET" >/dev/null 2>&1; then echo "already exists: $DATASET"; exit 0; fi
zfs create -o mountpoint="$MOUNT" -o compression=off -o recordsize=1M "$DATASET"
chown plex:plex "$MOUNT"; chmod 0755 "$MOUNT"
echo "created $DATASET -> $MOUNT"
ADDMOVIE
sed -i "s|__POOL__|${APP_POOL:-tank}|g; s|__MEDIA__|${PLEX_MEDIA_DIR}|g" /usr/local/bin/add-movie
chmod 0755 /usr/local/bin/add-movie

# ─── firewall, service, verify ──────────────────────────────────────────────
# 32400 web/API, 3005 companion, 8324 Roku, 32469 DLNA; 1900+32410-14 discovery.
app_firewall plex "$PLEX_ALLOW_CIDR" \
    32400/tcp 3005/tcp 8324/tcp 32469/tcp \
    1900/udp 5353/udp 32410/udp 32412/udp 32413/udp 32414/udp

systemctl daemon-reload
app_enable plexmediaserver

app_log "waiting for Plex on :32400"
app_wait_http http://127.0.0.1:32400/identity 90 ||
    app_warn "not answering yet — journalctl -u plexmediaserver -n 100"

# The claim token is single-use and short-lived. Do not leave it on disk.
if [ -f /etc/systemd/system/plexmediaserver.service.d/20-claim.conf ]; then
    rm -f /etc/systemd/system/plexmediaserver.service.d/20-claim.conf
    systemctl daemon-reload
fi

echo
app_check "plex answers on :32400"   curl -fsS --max-time 5 http://127.0.0.1:32400/identity
app_check "plexmediaserver enabled"  systemctl is-enabled plexmediaserver
app_check "plexmediaserver active"   systemctl is-active plexmediaserver
app_check "library mountpoint"       test -d "$PLEX_MEDIA_DIR/movies"
app_check "add-movie installed"      test -x /usr/local/bin/add-movie
if [ -n "${APP_POOL:-}" ]; then
    app_check "config recordsize 8K"  bash -c '[ "$(zfs get -H -o value recordsize '"$APP_POOL"'/plex/config)" = 8K ]'
    app_check "transcode not snapshotted" bash -c '[ "$(zfs get -H -o value com.sun:auto-snapshot '"$APP_POOL"'/plex/transcode)" = false ]'
    app_snapshot postinstall-plex
fi

cat <<EOM

  Plex on ZFS

  Web UI      http://$(hostname -I 2>/dev/null | awk '{print $1}'):32400/web
  Library     ${PLEX_MEDIA_DIR}      pool: ${APP_POOL:-<none — plain dirs>}
  Add a film  add-movie "The Matrix" 1999
  Firewall    zone 'plex', source ${PLEX_ALLOW_CIDR}

EOM
app_summary
`

// ─── DNS: AdGuard Home ───────────────────────────────────────────────
//
// The one appliance here that wants to own a privileged port on the whole
// network, which makes port 53 the entire risk surface: something else in
// the guest binding it first is the difference between a working lab DNS
// and a VM that quietly resolves nothing.

var adguardHome = Appliance{
	Name:     "AdGuard Home",
	Summary:  "Network-wide DNS ad and tracker blocking, with DoH/DoT",
	Homepage: "https://adguard.com/adguard-home.html",
	License:  "GPL-3.0",

	Distro: "debian",
	VCPUs:  1,
	RAMMB:  1024,
	DiskGB: 10,

	Needs: NeedsKVM,

	Port:    3000,
	LandsOn: "http://<vm-ip>:3000/  (first-run wizard; DNS on port 53)",

	Notes: "A pinned release, verified against upstream's published SHA-256, " +
		"registered as a systemd service by AdGuard's own installer.\n\n" +
		"SETUP: the first-run wizard at :3000 is where you set the admin " +
		"account and finish configuration. It is NOT scripted here on " +
		"purpose — the password is stored as a bcrypt hash that AdGuard's " +
		"own tooling generates, and faking that in bash produces an " +
		"appliance nobody can log into. Do the wizard now, not later: until " +
		"you do, the console is open to anyone who can reach the VM.\n\n" +
		"DNS: the script frees port 53 by disabling systemd-resolved's stub " +
		"listener if it is running, and fails loudly if anything else still " +
		"holds the port. Point your router's DHCP at this VM's address once " +
		"the wizard is done — and give the VM a static lease first, because " +
		"a DNS server that changes address takes the network with it.",

	Fields: nil,

	Script: adguardScript,
}

const adguardScript = `AGH_VERSION='v0.107.78'
AGH_SHA256_amd64='2070f644644be8299232f4a7bff857036fb1423563c1bf8c787e07aaf4f88278'
AGH_SHA256_arm64='71ef6d495d6d3fae45e6a80a172d44ae7f5aa528794cf927bb52fd5bff034eae'

export DEBIAN_FRONTEND=noninteractive

case "$(uname -m)" in
    x86_64)
        agh_arch=linux_amd64
        agh_sha="$AGH_SHA256_amd64"
        ;;
    aarch64 | arm64)
        agh_arch=linux_arm64
        agh_sha="$AGH_SHA256_arm64"
        ;;
    *)
        echo "FATAL: no AdGuard Home build for $(uname -m)" >&2
        exit 1
        ;;
esac

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl

# ─── Free port 53 before anything wants it ───────────────────────────
# A stub resolver on 127.0.0.53 owns :53 and AdGuard's installer would
# fail at bind time with a message that reads like a bug in AdGuard.
# Disabling the stub while keeping resolved's config sane means writing
# a real resolv.conf, or the guest loses name resolution mid-install.
if systemctl is-active --quiet systemd-resolved; then
    echo "adguard: disabling the systemd-resolved stub listener to free :53"
    install -d -m 0755 /etc/systemd/resolved.conf.d
    cat >/etc/systemd/resolved.conf.d/10-adguard-no-stub.conf <<'EOF'
# AdGuard Home needs port 53 on every address. The stub listener holds
# 127.0.0.53:53, so it is turned off here; resolv.conf is pointed at a
# public resolver so the guest can still resolve names while AdGuard is
# being installed and configured.
[Resolve]
DNSStubListener=no
EOF
    systemctl restart systemd-resolved
    rm -f /etc/resolv.conf
    printf 'nameserver 9.9.9.9\nnameserver 1.1.1.1\n' >/etc/resolv.conf
fi

# Whatever the cause, refusing to continue beats installing a DNS server
# that can never bind. ss is in iproute2, which the cloud image has.
if ss -lnup 2>/dev/null | grep -q ':53 ' || ss -lntp 2>/dev/null | grep -q ':53 '; then
    echo "FATAL: something is already listening on port 53:" >&2
    ss -lnup 2>/dev/null | grep ':53 ' >&2 || true
    ss -lntp 2>/dev/null | grep ':53 ' >&2 || true
    exit 1
fi

# ─── Fetch and verify ────────────────────────────────────────────────
tmpd="$(mktemp -d)"
trap 'rm -rf "$tmpd"' EXIT
curl -fsSL -o "$tmpd/agh.tar.gz" \
    "https://github.com/AdguardTeam/AdGuardHome/releases/download/${AGH_VERSION}/AdGuardHome_${agh_arch}.tar.gz"
echo "${agh_sha}  ${tmpd}/agh.tar.gz" | sha256sum -c -

tar -C /opt -xzf "$tmpd/agh.tar.gz"

# -s install registers and starts AdGuard's own systemd unit. Running it
# from the unpacked directory is upstream's documented invocation.
/opt/AdGuardHome/AdGuardHome -s install

for _ in $(seq 1 30); do
    if curl -fsS -o /dev/null "http://127.0.0.1:3000/" 2>/dev/null; then
        echo "adguard: setup wizard is up on 3000 — finish it now"
        exit 0
    fi
    sleep 5
done
echo "FATAL: AdGuard Home did not answer on 3000 after 2.5 minutes" >&2
systemctl --no-pager --full status AdGuardHome >&2 || true
exit 1
`

// ─── Files: Syncthing ────────────────────────────────────────────────
//
// Syncthing writes its own config on first start and there is no supported
// way to pre-seed it, so this installs, starts, waits for the config to
// exist, then edits the one value that matters (the GUI binds to loopback
// by default, which on a headless VM means nobody can ever reach it).

var syncthing = Appliance{
	Name:     "Syncthing",
	Summary:  "Continuous file sync between your own machines — no server, no cloud",
	Homepage: "https://syncthing.net",
	License:  "MPL-2.0",

	Distro: "debian",
	VCPUs:  1,
	RAMMB:  1024,
	DiskGB: 40,

	Needs:  NeedsZFS,
	DataGB: 100,

	Port:    8384,
	LandsOn: "http://<vm-ip>:8384/  (set a GUI password immediately)",

	Notes: "Installed from Syncthing's own Debian repository, whose signing " +
		"key is checked against a pinned fingerprint before it is trusted.\n\n" +
		"It runs as a dedicated syncthing user with its data under " +
		"/var/lib/syncthing, so this VM is a sync NODE for your other " +
		"machines rather than someone's desktop.\n\n" +
		"THE GUI STARTS WITHOUT A PASSWORD. Syncthing generates its config " +
		"on first start and stores GUI credentials hashed, so there is no " +
		"honest way to pre-seed an account from a script — the default bind " +
		"to 127.0.0.1 is changed to 0.0.0.0 here so you can reach it at all. " +
		"Open it and set a username and password under Actions → Settings " +
		"→ GUI before anything else. Until you do, anyone who can reach " +
		"port 8384 can add folders to it.",

	Fields: []ApplianceField{
		{Key: "ST_POOL", Label: "pool name", Default: "tank", Required: true},
		{Key: "ST_DATA_DIR", Label: "shared folder directory",
			Placeholder: "where synced folders live",
			Default:     "/srv/sync", Required: true},
	},

	Validate: func(v map[string]string) error {
		return checkAbsDir(v["ST_DATA_DIR"], "shared folder directory")
	},

	Script: syncthingScript,
}

const syncthingScript = `
APP_TAG=syncthing
APP_POOL="$ST_POOL"
app_pool_init
# The config DB is small and precious; the SYNCED DATA is the reason ZFS is
# here at all. Syncthing propagates deletions by design, so a fat-fingered
# rm on one machine reaches every machine — a snapshot is what makes that a
# five-second rollback instead of a restore-from-backup. sync/ is a
# canmount=off container: add-share gives each shared folder its own dataset,
# so one folder rolls back, sends or quotas without touching the others.
app_dataset syncthing /var/lib/syncthing recordsize=16K
app_dataset sync      /srv/sync          canmount=off
mkdir -p /srv/sync

# add-share <name> — one dataset per shared folder, the per-title model.
cat >/usr/local/bin/add-share <<'ADDSHARE'
#!/usr/bin/env bash
# add-share <name> — a snapshot-isolated dataset for one Syncthing folder.
set -Eeuo pipefail
NAME="${1:-}"
[ -n "$NAME" ] || { echo "usage: add-share <name>" >&2; exit 2; }
SLUG=$(printf '%s' "$NAME" | tr '[:upper:]' '[:lower:]' | tr ' ' - | tr -cd 'a-z0-9-')
[ -n "$SLUG" ] || { echo "name produced an empty slug" >&2; exit 2; }
DS="__POOL__/sync/$SLUG"; MNT="/srv/sync/$SLUG"
if zfs list "$DS" >/dev/null 2>&1; then echo "already exists: $DS"; exit 0; fi
zfs create -o mountpoint="$MNT" "$DS"
chown syncthing:syncthing "$MNT"; chmod 0770 "$MNT"
echo "created $DS -> $MNT — add it as a folder in the Syncthing UI"
ADDSHARE
sed -i "s|__POOL__|${APP_POOL:-tank}|g" /usr/local/bin/add-share
chmod 0755 /usr/local/bin/add-share
ST_KEY_URL='https://syncthing.net/release-key.gpg'
ST_KEY_FPR='FBA2E162F2F44657B38F0309E5665F9BD5970C47'

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg

# ─── Trust, but verify ───────────────────────────────────────────────
install -d -m 0755 /etc/apt/keyrings
tmpkey="$(mktemp)"
trap 'rm -f "$tmpkey"' EXIT
curl -fsSL "$ST_KEY_URL" -o "$tmpkey"

got_fpr="$(gpg --show-keys --with-colons --with-fingerprint "$tmpkey" |
    awk -F: '$1 == "fpr" { print $10; exit }')"
if [[ "$got_fpr" != "$ST_KEY_FPR" ]]; then
    echo "FATAL: Syncthing signing key fingerprint mismatch" >&2
    echo "  expected $ST_KEY_FPR" >&2
    echo "  got      ${got_fpr:-<none>}" >&2
    exit 1
fi
gpg --dearmor --yes -o /etc/apt/keyrings/syncthing.gpg "$tmpkey"
chmod 0644 /etc/apt/keyrings/syncthing.gpg

deb_arch="$(dpkg --print-architecture)"
cat >/etc/apt/sources.list.d/syncthing.list <<EOF
deb [arch=${deb_arch} signed-by=/etc/apt/keyrings/syncthing.gpg] https://apt.syncthing.net/ syncthing stable
EOF

apt-get update
apt-get install -y syncthing

# ─── A service account, not somebody's desktop session ───────────────
adduser --system --group --disabled-password --shell /usr/sbin/nologin \
    --home /var/lib/syncthing --gecos 'Syncthing' syncthing
install -d -m 0750 -o syncthing -g syncthing /var/lib/syncthing
install -d -m 0770 -o syncthing -g syncthing "$ST_DATA_DIR"

# The packaged syncthing@.service template runs as the named user with
# that user's home as the config dir, which is exactly this layout.
systemctl enable --now syncthing@syncthing.service

# ─── Reach the GUI at all ────────────────────────────────────────────
# Syncthing writes config.xml on first start and binds the GUI to
# 127.0.0.1. On a headless VM that means the web interface exists and is
# unreachable forever. There is no pre-seed path, so: wait for the file
# it just wrote, change the one address, restart.
cfg=/var/lib/syncthing/.local/state/syncthing/config.xml
for _ in $(seq 1 30); do
    [[ -f "$cfg" ]] && break
    # Older packages keep it under ~/.config/syncthing instead.
    if [[ -f /var/lib/syncthing/.config/syncthing/config.xml ]]; then
        cfg=/var/lib/syncthing/.config/syncthing/config.xml
        break
    fi
    sleep 2
done
if [[ ! -f "$cfg" ]]; then
    echo "FATAL: syncthing never wrote a config; the GUI would be unreachable" >&2
    systemctl --no-pager --full status syncthing@syncthing.service >&2 || true
    exit 1
fi

cp -a "$cfg" "${cfg}.vmxplore-orig"
sed -i 's|<address>127\.0\.0\.1:8384</address>|<address>0.0.0.0:8384</address>|' "$cfg"
systemctl restart syncthing@syncthing.service

for _ in $(seq 1 30); do
    if curl -fsS -o /dev/null "http://127.0.0.1:8384/" 2>/dev/null; then
        echo "syncthing: GUI up on 8384 — set a password before anything else"
        exit 0
    fi
    sleep 5
done
echo "FATAL: syncthing GUI did not answer on 8384" >&2
systemctl --no-pager --full status syncthing@syncthing.service >&2 || true
exit 1
`

// ─── Shared validation ───────────────────────────────────────────────

// applianceEmailRE is deliberately loose: it rejects the typo ("gitadmin",
// "you@") that an app would only complain about from inside the guest, and
// does not attempt to be RFC 5322 — that fight has no winners and would
// reject addresses that work.
var applianceEmailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// checkAbsDir validates an operator-supplied directory path.
//
// Args:   p     the path as typed; what  the field name, for the message.
// Returns: nil, or an error naming the field.
//
// WHY: these paths are handed to `install -d` and to service config as
// root. A relative path silently creates a directory wherever cloud-init
// happened to be running, and the operator finds an empty library and a
// mystery directory under / much later.
func checkAbsDir(p, what string) error {
	switch {
	case strings.TrimSpace(p) == "":
		return fmt.Errorf("%s is required", what)
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("%s %q must be an absolute path", what, p)
	case strings.Contains(p, ".."):
		return fmt.Errorf("%s %q must not contain '..'", what, p)
	}
	return nil
}

// ─── Seedbox ─────────────────────────────────────────────────────────────
//
// The site's Automated Seedbox recipe, as a tile: qBittorrent on the same
// per-title dataset model as the media servers, a landing dataset for
// in-flight downloads, and — when VPN details are supplied — a WireGuard
// tunnel with an nftables KILL SWITCH so nothing leaks if the tunnel drops.
// The recipe calls the kill switch non-negotiable, and it is right.
var seedbox = Appliance{
	Name:     "Seedbox",
	Summary:  "qBittorrent on tuned datasets, with a VPN kill switch that fails closed",
	Homepage: "https://www.qbittorrent.org",
	License:  "GPL-2.0 (qBittorrent)",

	Needs:  NeedsZFS,
	DataGB: 500,

	Distro: "fedora",
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 20,

	Port:    8080,
	LandsOn: "http://<vm-ip>:8080/  (qBittorrent web UI; password printed in the build log)",

	Notes: "Downloads land on a landing dataset (lz4, 1M records) and get " +
		"sorted into the same media/movies, media/tv per-title containers " +
		"the Plex and Jellyfin tiles use — so a finished download is one " +
		"zfs send away from the media server.\n\n" +
		"THE KILL SWITCH: fill in the four VPN fields and ALL outbound " +
		"traffic is forced through wg0 — if the tunnel drops, torrent " +
		"traffic stops rather than leaking your address. The web UI stays " +
		"reachable from your LAN CIDR. Leave the VPN fields empty and " +
		"torrents go out directly, kill switch off.\n\n" +
		"The VPN private key rides in the cloud-init seed; treat it as a " +
		"bootstrap credential and rotate it with your provider if the seed " +
		"ISO leaves the host.",

	Fields: []ApplianceField{
		{Key: "SB_POOL", Label: "pool name",
			Default: "tank", Required: true},
		{Key: "SB_ALLOW_CIDR", Label: "allowed source",
			Placeholder: "who may reach the web UI",
			Default:     "192.168.0.0/16", Required: true},
		{Key: "SB_VPN_ADDRESS", Label: "VPN address (optional)",
			Placeholder: "e.g. 10.2.0.2/32 from your provider"},
		{Key: "SB_VPN_PRIVKEY", Label: "VPN private key (optional)",
			Placeholder: "wg private key from your provider"},
		{Key: "SB_VPN_PEER_PUB", Label: "VPN peer public key (optional)",
			Placeholder: "the provider endpoint's public key"},
		{Key: "SB_VPN_ENDPOINT", Label: "VPN endpoint (optional)",
			Placeholder: "host:port"},
	},

	Validate: func(v map[string]string) error {
		if err := checkPoolName(v["SB_POOL"]); err != nil {
			return err
		}
		vpn := 0
		for _, k := range []string{"SB_VPN_ADDRESS", "SB_VPN_PRIVKEY", "SB_VPN_PEER_PUB", "SB_VPN_ENDPOINT"} {
			if v[k] != "" {
				vpn++
			}
		}
		if vpn != 0 && vpn != 4 {
			return fmt.Errorf("VPN needs all four fields (address, private key, peer public key, endpoint) — %d of 4 set", vpn)
		}
		return nil
	},

	Script: seedboxScript,
}

const seedboxScript = `
APP_TAG=seedbox
APP_POOL="$SB_POOL"

app_pool_init

# ─── datasets, per the seedbox recipe ───────────────────────────────────────
# landing: in-flight downloads — big sequential writes, lz4 catches the
# compressible stragglers, and it is never snapshotted: everything here is
# either re-downloadable or about to be sorted into media/.
app_dataset landing /srv/landing recordsize=1M compression=lz4 \
    com.sun:auto-snapshot=false
# media: identical shape to the media-server tiles, so a finished item is one
# rename (same pool) or one zfs send (their pool) from the library.
app_dataset media        none              canmount=off
app_dataset media/movies /srv/media/movies canmount=off
app_dataset media/tv     /srv/media/tv     canmount=off
# session state: many small resume files, rewritten constantly.
app_dataset apps         /opt/seedbox      compression=lz4
app_dataset apps/session /opt/seedbox/session recordsize=16K
mkdir -p /srv/media/movies /srv/media/tv

# ─── qBittorrent ────────────────────────────────────────────────────────────
if [ "$APP_FAMILY" = rpm ]; then app_pkg qbittorrent-nox; else app_pkg qbittorrent-nox; fi
getent passwd seedbox >/dev/null || useradd -r -d /opt/seedbox -s /usr/sbin/nologin seedbox
chown -R seedbox:seedbox /srv/landing /srv/media /opt/seedbox

install -d -m 0750 -o seedbox -g seedbox /opt/seedbox/.config/qBittorrent
cat >/opt/seedbox/.config/qBittorrent/qBittorrent.conf <<QBT
[BitTorrent]
Session\DefaultSavePath=/srv/landing
Session\TempPath=/srv/landing/incomplete
Session\Port=52630
QBT
chown -R seedbox:seedbox /opt/seedbox/.config

cat >/etc/systemd/system/qbittorrent-nox.service <<UNIT
[Unit]
Description=qBittorrent (headless)
After=network-online.target zfs-mount.service wg-quick@wg0.service
Wants=network-online.target
RequiresMountsFor=/srv/landing /opt/seedbox

[Service]
User=seedbox
Group=seedbox
Environment=HOME=/opt/seedbox
ExecStart=/usr/bin/qbittorrent-nox --webui-port=8080 --profile=/opt/seedbox
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes

[Install]
WantedBy=multi-user.target
UNIT

# ─── VPN + kill switch, only when the operator supplied a tunnel ────────────
if [ -n "${SB_VPN_PRIVKEY:-}" ]; then
    app_pkg wireguard-tools nftables
    _ep_host="${SB_VPN_ENDPOINT%:*}"
    _ep_port="${SB_VPN_ENDPOINT##*:}"
    install -d -m 0700 /etc/wireguard
    cat >/etc/wireguard/wg0.conf <<WG
[Interface]
PrivateKey = ${SB_VPN_PRIVKEY}
Address = ${SB_VPN_ADDRESS}

[Peer]
PublicKey = ${SB_VPN_PEER_PUB}
Endpoint = ${SB_VPN_ENDPOINT}
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
WG
    chmod 0600 /etc/wireguard/wg0.conf
    app_enable wg-quick@wg0

    # The kill switch: outbound is wg0, the LAN, DHCP/DNS bootstrap, and the
    # tunnel handshake itself — nothing else. If wg0 drops, torrent traffic
    # stops instead of leaking. Input stays open to the LAN so the web UI
    # cannot lock you out.
    cat >/etc/nftables-killswitch.conf <<NFT
table inet killswitch {
    chain output {
        type filter hook output priority 0; policy drop;
        oifname "lo" accept
        oifname "wg0" accept
        # ap-* is the kldload MANAGEMENT mesh (enrollment puts every appliance
        # on one). It carries Ansible and operator ssh, never torrent traffic
        # -- AllowedIPs on that plane are /32 mesh addresses only -- and a kill
        # switch that severs management has failed the wrong way.
        oifname "ap-*" accept
        ct state established,related accept
        ip daddr ${SB_ALLOW_CIDR} accept
        udp dport { 53, 67, 68 } accept
        ip daddr ${_ep_host} udp dport ${_ep_port} accept
        log prefix "killswitch-blocked: " limit rate 1/minute
    }
}
NFT
    if [ "$APP_FAMILY" = rpm ]; then
        _nftmain=/etc/sysconfig/nftables.conf
    else
        _nftmain=/etc/nftables.conf
    fi
    grep -q nftables-killswitch "$_nftmain" 2>/dev/null ||
        echo 'include "/etc/nftables-killswitch.conf"' >>"$_nftmain"
    nft -f /etc/nftables-killswitch.conf || app_die "kill switch rules did not load"
    app_enable nftables
    # Bind the session to wg0 so torrents cannot even try another interface.
    printf 'Session\\Interface=wg0\nSession\\InterfaceName=wg0\n' \
        >>/opt/seedbox/.config/qBittorrent/qBittorrent.conf
    app_log "kill switch armed: outbound is wg0-or-nothing"
else
    app_warn "no VPN configured — torrents will use the VM's own address"
fi

# ─── firewall, service, verify ──────────────────────────────────────────────
app_firewall seedbox "$SB_ALLOW_CIDR" 8080/tcp
systemctl daemon-reload
app_enable qbittorrent-nox

app_log "waiting for the web UI on :8080"
app_wait_http http://127.0.0.1:8080/ 90 ||
    app_warn "web UI not answering — journalctl -u qbittorrent-nox -n 50"

echo
app_check "qbittorrent answers on :8080" curl -fsS --max-time 5 http://127.0.0.1:8080/
app_check "qbittorrent enabled"          systemctl is-enabled qbittorrent-nox
app_check "landing dataset mounted"      mountpoint -q /srv/landing
app_check "media containers present"     test -d /srv/media/movies
if [ -n "${SB_VPN_PRIVKEY:-}" ]; then
    app_check "wg0 is up"                bash -c 'wg show wg0 2>/dev/null | grep -q .'
    app_check "kill switch loaded"       bash -c 'nft list table inet killswitch >/dev/null'
fi
if [ -n "${APP_POOL:-}" ]; then
    app_check "landing never snapshotted" bash -c '[ "$(zfs get -H -o value com.sun:auto-snapshot "$APP_POOL"/landing)" = false ]'
    app_snapshot postinstall-seedbox
fi

_qbtpass="$(journalctl -u qbittorrent-nox 2>/dev/null | grep -oE 'temporary password.*: .*' | tail -1)"
cat <<EOM

  Seedbox

  Web UI      http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080/
              login: admin — ${_qbtpass:-password in journalctl -u qbittorrent-nox}
  Landing     /srv/landing        Media  /srv/media/{movies,tv}
  Kill switch $([ -n "${SB_VPN_PRIVKEY:-}" ] && echo "ARMED (wg0-or-nothing)" || echo "off — no VPN supplied")

EOM
app_summary
`

// ─── Icecast station rack ────────────────────────────────────────────────
//
// Written for abyss: the FreeBSD box that runs thirty music streams, on its
// way to kldload. The model is one icecast INSTANCE per station — its own
// unit, its own port, its own dataset — so one station's restart, quota or
// migration never touches the other twenty-nine. add-station stamps out
// number thirty-one.
var icecast = Appliance{
	Name:     "Icecast Stations",
	Summary:  "A rack of independent Icecast servers — one unit, port and dataset per station",
	Homepage: "https://icecast.org",
	License:  "GPL-2.0",

	Needs:  NeedsZFS,
	DataGB: 100,

	Distro: "fedora",
	VCPUs:  2,
	RAMMB:  1024,
	DiskGB: 15,

	Port:    8001,
	LandsOn: "http://<vm-ip>:8001/  (station 1; station N is on 8000+N)",

	Notes: "Each station is a separate icecast process: icecast@N.service " +
		"listening on 8000+N, configured from /etc/icecast-stations/N.xml, " +
		"with logs and state on its own dataset. Thirty stations means " +
		"thirty units — systemctl restart icecast@7 touches ONE stream.\n\n" +
		"add-station <name> creates the next one: dataset, config, unit, " +
		"firewall. The source password is per-rack (one encoder fleet), the " +
		"admin password likewise; both are in the fields.\n\n" +
		"Stream sources (liquidsoap, ices, BUTT) point at " +
		"<vm-ip>:800N with the source password.",

	Fields: []ApplianceField{
		{Key: "IC_POOL", Label: "pool name",
			Default: "tank", Required: true},
		{Key: "IC_STATIONS", Label: "stations to create now",
			Placeholder: "1-64; add-station makes more later",
			Default:     "4", Required: true},
		{Key: "IC_ALLOW_CIDR", Label: "listener source",
			Placeholder: "who may tune in",
			Default:     "192.168.0.0/16", Required: true},
		{Key: "IC_ADMIN_PASS", Label: "admin password",
			Placeholder: "blank = generate one", Secret: true,
			Generate: true, Required: true},
		{Key: "IC_SOURCE_PASS", Label: "source (encoder) password",
			Placeholder: "blank = generate one", Secret: true,
			Generate: true, Required: true},
	},

	Validate: func(v map[string]string) error {
		if err := checkPoolName(v["IC_POOL"]); err != nil {
			return err
		}
		n, err := strconv.Atoi(v["IC_STATIONS"])
		if err != nil || n < 1 || n > 64 {
			return fmt.Errorf("stations must be a number from 1 to 64")
		}
		for _, k := range []string{"IC_ADMIN_PASS", "IC_SOURCE_PASS"} {
			if len(v[k]) < 8 {
				return fmt.Errorf("%s must be at least 8 characters", k)
			}
			if strings.ContainsAny(v[k], " \t\n'\"<>&") {
				return fmt.Errorf("%s must avoid spaces, quotes and XML characters", k)
			}
		}
		return nil
	},

	Script: icecastScript,
}

const icecastScript = `
APP_TAG=icecast
APP_POOL="$IC_POOL"

app_pool_init

# stations/ is a container; every station below it is its own dataset, so a
# station moves between pools — or between MACHINES, via zfs send — alone.
app_dataset stations /srv/stations canmount=off

if [ "$APP_FAMILY" = rpm ]; then
    app_pkg icecast
    _icuser=icecast
else
    # icecast2 tries a debconf dialog; the noninteractive frontend from the
    # substrate suppresses it, and the packaged single-instance service is
    # disabled because the rack model replaces it.
    app_pkg icecast2
    _icuser=icecast2
    systemctl disable --now icecast2 2>/dev/null || true
fi

install -d -m 0755 /etc/icecast-stations

# The template unit: one icecast per station, numbered.
cat >/etc/systemd/system/icecast@.service <<UNIT
[Unit]
Description=Icecast station %i
After=network-online.target zfs-mount.service
Wants=network-online.target
RequiresMountsFor=/srv/stations/%i

[Service]
User=${_icuser}
Group=${_icuser}
ExecStart=/usr/bin/icecast -c /etc/icecast-stations/%i.xml
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes

[Install]
WantedBy=multi-user.target
UNIT
[ -x /usr/bin/icecast ] || sed -i 's|/usr/bin/icecast |/usr/bin/icecast2 |' /etc/systemd/system/icecast@.service

# ─── add-station: stamp out the next one ────────────────────────────────────
cat >/usr/local/bin/add-station <<'ADDST'
#!/usr/bin/env bash
# add-station <n> [name] — dataset, config, unit and port 8000+n for one station.
set -Eeuo pipefail
N="${1:-}"; NAME="${2:-station-$N}"
[ -n "$N" ] && [ "$N" -ge 1 ] 2>/dev/null || { echo "usage: add-station <n> [name]" >&2; exit 2; }
PORT=$((8000 + N))
DS="__POOL__/stations/$N"
MNT="/srv/stations/$N"
if ! zfs list "$DS" >/dev/null 2>&1 && command -v zfs >/dev/null 2>&1 && [ -n "__POOL__" ]; then
    zfs create -o mountpoint="$MNT" -o compression=lz4 "$DS"
fi
mkdir -p "$MNT/log" "$MNT/web"
cat >/etc/icecast-stations/$N.xml <<XML
<icecast>
  <hostname>$(hostname)</hostname>
  <location>__NAME__ rack</location>
  <limits><clients 	>100</clients><sources>4</sources></limits>
  <authentication>
    <source-password>__SRCPASS__</source-password>
    <admin-user>admin</admin-user>
    <admin-password>__ADMPASS__</admin-password>
  </authentication>
  <listen-socket><port>$PORT</port></listen-socket>
  <paths>
    <basedir>$MNT</basedir>
    <logdir>$MNT/log</logdir>
    <webroot>/usr/share/icecast/web</webroot>
    <adminroot>/usr/share/icecast/admin</adminroot>
    <alias source="/" destination="/status.xsl"/>
  </paths>
  <logging>
    <accesslog>access.log</accesslog><errorlog>error.log</errorlog>
    <loglevel>3</loglevel>
  </logging>
  <security><chroot>0</chroot></security>
</icecast>
XML
# Debian keeps the web assets under icecast2.
[ -d /usr/share/icecast2/web ] && sed -i 's|/usr/share/icecast/|/usr/share/icecast2/|g' /etc/icecast-stations/$N.xml
chown -R __ICUSER__:__ICUSER__ "$MNT"
chown __ICUSER__:__ICUSER__ /etc/icecast-stations/$N.xml
chmod 0640 /etc/icecast-stations/$N.xml
systemctl enable --now "icecast@$N.service"
command -v firewall-cmd >/dev/null 2>&1 &&
    firewall-cmd --permanent --zone=icecast --add-port=$PORT/tcp >/dev/null 2>&1 &&
    firewall-cmd --reload >/dev/null 2>&1
echo "station $N ($NAME) on :$PORT — source pw as configured, mount with any encoder"
ADDST
sed -i "s|__POOL__|${APP_POOL:-}|g; s|__ICUSER__|${_icuser}|g; s|__SRCPASS__|${IC_SOURCE_PASS}|g; s|__ADMPASS__|${IC_ADMIN_PASS}|g; s|__NAME__|$(hostname)|g" /usr/local/bin/add-station
chmod 0750 /usr/local/bin/add-station

# The odd-looking tab in "<clients 	>" above would be an icecast config
# error; normalise it here rather than risk a template typo shipping.
sed -i 's|<clients 	>|<clients>|' /usr/local/bin/add-station

# ─── build the initial rack ─────────────────────────────────────────────────
_ports=""
_n=1
while [ "$_n" -le "$IC_STATIONS" ]; do
    /usr/local/bin/add-station "$_n" || app_warn "station $_n failed"
    _ports="$_ports $((8000 + _n))/tcp"
    _n=$((_n + 1))
done
# shellcheck disable=SC2086  # port list is built above, one word each
app_firewall icecast "$IC_ALLOW_CIDR" $_ports

echo
_up=0; _n=1
while [ "$_n" -le "$IC_STATIONS" ]; do
    systemctl is-active "icecast@$_n" >/dev/null 2>&1 && _up=$((_up + 1))
    _n=$((_n + 1))
done
app_check "all stations active ($_up/$IC_STATIONS)" [ "$_up" -eq "$IC_STATIONS" ]
app_check "station 1 answers"  app_wait_http http://127.0.0.1:8001/status.xsl 30
app_check "add-station installed" test -x /usr/local/bin/add-station
[ -n "${APP_POOL:-}" ] && app_snapshot postinstall-icecast

cat <<EOM

  Icecast Stations

  Stations    ${IC_STATIONS}, on ports 8001-$((8000 + IC_STATIONS))
  Status      http://$(hostname -I 2>/dev/null | awk '{print $1}'):8001/
  Add more    add-station $((IC_STATIONS + 1)) jazz-after-dark
  Encoders    point at <vm-ip>:800N, source password as configured

EOM
app_summary
`

// ─── SDR Station ─────────────────────────────────────────────────────────
//
// A proper SDR receiver as an appliance: OpenWebRX+ (web SDR on :8073, any
// browser becomes a spectrum display), SoapySDR with a SoapyRemote server so
// desktop clients (SDR++, GQRX) can use this box's radios over the network,
// and the raw hackrf/rtl-sdr tooling for captures. IQ recordings land on
// their own dataset — high-entropy data, so compression stays off and a
// quota keeps a runaway capture from eating the pool.
var sdrStation = Appliance{
	Name:     "SDR Station",
	Summary:  "OpenWebRX + SoapyRemote around a HackRF/RTL-SDR — the estate's antenna socket",
	Homepage: "https://www.openwebrx.de",
	License:  "AGPL-3.0 (OpenWebRX), GPL-2.0 (hackrf, rtl-sdr)",

	Needs:  NeedsZFS,
	DataGB: 200,

	Distro: "debian-bookworm", // OpenWebRX+ pins python3<3.10; trixie ships 3.13
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 15,

	Port:    8073,
	LandsOn: "http://<vm-ip>:8073/  (waterfall in the browser; SoapyRemote for SDR++/GQRX on :55132)",

	// The radios ride USB passthrough: attached live after the build, so the
	// recipe's own hardware checks can already see them, and persistent so a
	// reboot keeps them.
	USB: []string{
		"1d50:6089", // HackRF One
		"1d50:604b", // HackRF Jawbreaker
		"0bda:2838", // RTL-SDR (RTL2838)
		"0bda:2832", // RTL-SDR (RTL2832)
		"1d50:60a1", // Airspy
	},

	Notes: "The radio must be plugged into THIS host when the tile builds — " +
		"absent devices are skipped with a warning naming the manual attach. " +
		"Plug in later with: virsh attach-device <vm> <hostdev.xml> " +
		"--persistent.\n\n" +
		"IQ captures are enormous (20 Msps complex = ~80 MB/s) and do not " +
		"compress; /srv/captures gets compression=off, 1M records and a " +
		"quota. hackrf_transfer and rtl_sdr write there.\n\n" +
		"OpenWebRX+ comes from its maintainer's repository over HTTPS; the " +
		"project publishes no key fingerprint to pin, which is noted at the " +
		"install site rather than hidden.\n\n" +
		"This tile runs on Debian 12 (bookworm) on purpose: OpenWebRX+ pins " +
		"python3<3.10 and trixie ships 3.13, so the web half will not resolve " +
		"there. SoapyRemote and the capture tools are distro-agnostic.",

	Fields: []ApplianceField{
		{Key: "SDR_POOL", Label: "pool name",
			Default: "tank", Required: true},
		{Key: "SDR_ALLOW_CIDR", Label: "allowed source",
			Placeholder: "who may reach the waterfall",
			Default:     "192.168.0.0/16", Required: true},
		{Key: "SDR_CAPTURE_QUOTA", Label: "captures quota",
			Placeholder: "e.g. 150G",
			Default:     "150G", Required: true},
		{Key: "SDR_ADMIN_PASS", Label: "openwebrx admin password",
			Placeholder: "blank = generate one", Secret: true,
			Generate: true, Required: true},
	},

	Validate: func(v map[string]string) error {
		return checkPoolName(v["SDR_POOL"])
	},

	Script: sdrStationScript,
}

const sdrStationScript = `
APP_TAG=sdr
APP_POOL="$SDR_POOL"

app_pool_init

# IQ is high-entropy: compression would burn CPU to save nothing. The quota is
# what stands between one forgotten hackrf_transfer and a full pool.
app_dataset captures /srv/captures recordsize=1M compression=off \
    quota="$SDR_CAPTURE_QUOTA" com.sun:auto-snapshot=false
# settings/bookmarks are tiny and precious — snapshot-worthy.
app_dataset sdr-config /var/lib/openwebrx recordsize=16K

# ─── radio tooling ──────────────────────────────────────────────────────────
app_pkg curl usbutils
if [ "$APP_FAMILY" = rpm ]; then
    app_pkg hackrf rtl-sdr soapy-utils soapy-hackrf soapy-rtlsdr
    app_warn "OpenWebRX has no Fedora packaging — Soapy/hackrf tools only on this family"
else
    app_pkg hackrf rtl-sdr soapysdr-tools soapysdr-module-hackrf soapysdr-module-rtlsdr
fi

# SoapyRemote: the estate-facing SDR server. Any SDR++/GQRX on the LAN can
# open this box's radio as if it were local.
if [ "$APP_FAMILY" = rpm ]; then app_pkg_optional soapy-remote; else app_pkg_optional soapyremote-server; fi
cat >/etc/systemd/system/soapyremote.service <<UNIT
[Unit]
Description=SoapySDR network server
After=network-online.target

[Service]
ExecStart=/usr/bin/SoapySDRServer --bind
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
UNIT

# ─── OpenWebRX+ (Debian family) ─────────────────────────────────────────────
if [ "$APP_FAMILY" = deb ]; then
    # SECURITY NOTE, stated rather than hidden: the OpenWebRX+ project
    # publishes its repo key over HTTPS at a pinned URL but no fingerprint to
    # verify against. HTTPS-from-the-vendor is the trust anchor here; if that
    # is not acceptable on your network, skip the web half — SoapyRemote and
    # the capture tools above carry no such caveat.
    install -d -m 0755 /etc/apt/keyrings
    if curl -fsSL https://luarvique.github.io/ppa/openwebrx-plus.gpg \
        -o /etc/apt/keyrings/openwebrx-plus.gpg; then
        . /etc/os-release
        echo "deb [signed-by=/etc/apt/keyrings/openwebrx-plus.gpg] https://luarvique.github.io/ppa/debian ./" \
            >/etc/apt/sources.list.d/openwebrx-plus.list
        apt-get update -qq >/dev/null 2>&1 || true
        if apt-get install -y -qq openwebrx >/var/log/appliance-openwebrx.log 2>&1; then
            app_log "openwebrx installed"
            if [ -n "${SDR_ADMIN_PASS:-}" ]; then
                OWRX_PASSWORD="$SDR_ADMIN_PASS" openwebrx admin --noninteractive adduser admin \
                    >/dev/null 2>&1 || app_warn "could not create the openwebrx admin user"
            fi
            app_enable openwebrx
        else
            app_warn "openwebrx did not install — tail of /var/log/appliance-openwebrx.log:"
            tail -3 /var/log/appliance-openwebrx.log >&2
        fi
    else
        app_warn "openwebrx repo key unreachable — web SDR skipped, Soapy tools remain"
    fi
fi

app_enable soapyremote

# ─── firewall, verify ───────────────────────────────────────────────────────
# 8073 web, 55132 SoapyRemote RPC (it negotiates data streams above it).
app_firewall sdr "$SDR_ALLOW_CIDR" 8073/tcp 55132/tcp

echo
app_check "SoapySDR sees its modules"  bash -c 'SoapySDRUtil --info 2>/dev/null | grep -qi module'
app_check "soapyremote enabled"        systemctl is-enabled soapyremote
if lsusb -d 1d50:6089 >/dev/null 2>&1 || lsusb -d 1d50:604b >/dev/null 2>&1; then
    app_check "HackRF answers"         bash -c 'hackrf_info 2>/dev/null | grep -qi "serial number"'
elif lsusb -d 0bda:2838 >/dev/null 2>&1 || lsusb -d 0bda:2832 >/dev/null 2>&1; then
    app_check "RTL-SDR answers"        bash -c 'rtl_test -t 2>&1 | grep -qi "found"'
else
    app_warn "no SDR device visible in the guest — passthrough missed or nothing plugged in"
fi
if [ "$APP_FAMILY" = deb ] && command -v openwebrx >/dev/null 2>&1; then
    app_check "openwebrx answers on :8073" app_wait_http http://127.0.0.1:8073/ 60
fi
app_check "captures dataset mounted"   mountpoint -q /srv/captures
[ -n "${APP_POOL:-}" ] && app_snapshot postinstall-sdr

cat <<EOM

  SDR Station

  Waterfall   http://$(hostname -I 2>/dev/null | awk '{print $1}'):8073/
  SoapyRemote <vm-ip>:55132   (SDR++/GQRX: driver=remote)
  Captures    /srv/captures (quota ${SDR_CAPTURE_QUOTA}, compression off)
  Try         hackrf_transfer -r /srv/captures/fm.iq -f 99500000 -s 10000000

EOM
app_summary
`

// ─── Tvheadend DVR ───────────────────────────────────────────────────────
//
// Live TV and satellite in one tile: Tvheadend speaks ATSC/DVB-T over-the-air
// AND DVB-S/S2 dishes, records to its own tuned dataset, and serves every TV
// in the house over HTTP. USB tuners ride the same passthrough as the SDR
// tile; a PCI capture card stays host-side vfio work and the Notes say so.
var tvheadend = Appliance{
	Name:     "Tvheadend DVR",
	Summary:  "Over-the-air + satellite DVR — tuners in, recordings on a tuned dataset, streams out",
	Homepage: "https://tvheadend.org",
	License:  "GPL-3.0",

	Needs:  NeedsZFS,
	DataGB: 500,

	Distro: "debian",
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 15,

	Port:    9981,
	LandsOn: "http://<vm-ip>:9981/  (setup wizard: tuners → mux scan → channels)",

	USB: []string{
		"2040:0265", // Hauppauge WinTV-dualHD (ATSC)
		"2040:826d", // Hauppauge WinTV-HVR-950Q
		"0bda:2838", // RTL2838 (DVB-T mode)
	},

	Notes: "Recordings are big sequential video: compression=off, 1M records, " +
		"and a quota so a season pass cannot eat the pool. Timeshift is " +
		"regenerable by definition — sync=disabled, never snapshotted.\n\n" +
		"A USB tuner plugged into this host attaches automatically. A PCI " +
		"capture card needs host-side vfio passthrough first (IOMMU on, " +
		"device bound to vfio-pci) — deliberate host configuration, not " +
		"something a tile springs on a machine. Once bound: " +
		"virsh attach-device with a pci hostdev.\n\n" +
		"Satellite: a DVB-S2 USB box appears the same way; point a mux scan " +
		"at your bird and Tvheadend does the rest.",

	Fields: []ApplianceField{
		{Key: "TVH_POOL", Label: "pool name",
			Default: "tank", Required: true},
		{Key: "TVH_ALLOW_CIDR", Label: "allowed source",
			Placeholder: "who may watch",
			Default:     "192.168.0.0/16", Required: true},
		{Key: "TVH_REC_QUOTA", Label: "recordings quota",
			Placeholder: "e.g. 400G",
			Default:     "400G", Required: true},
	},

	Validate: func(v map[string]string) error {
		return checkPoolName(v["TVH_POOL"])
	},

	Script: tvheadendScript,
}

const tvheadendScript = `
APP_TAG=tvheadend
APP_POOL="$TVH_POOL"

app_pool_init

app_dataset recordings /srv/recordings recordsize=1M compression=off \
    quota="$TVH_REC_QUOTA"
app_dataset timeshift /srv/timeshift recordsize=1M compression=off \
    sync=disabled quota=50G com.sun:auto-snapshot=false
app_dataset tvh-config /var/lib/tvheadend recordsize=16K

app_pkg curl usbutils
if [ "$APP_FAMILY" = rpm ]; then
    app_die "tvheadend has no usable Fedora packaging — build this tile on the Debian image"
fi

# non-free-firmware carries most tuner firmware blobs; trixie cloud images
# already list the component, so this is usually a no-op.
sed -i '/^Components:/ { /non-free-firmware/! s/$/ non-free-firmware/ }' \
    /etc/apt/sources.list.d/*.sources 2>/dev/null || true

# Tvheadend's own repo (cloudsmith). Key over HTTPS from the project's
# documented URL; like the SDR tile, the caveat is stated: no published
# fingerprint exists to pin against.
install -d -m 0755 /etc/apt/keyrings
curl -fsSL "https://dl.cloudsmith.io/public/tvheadend/tvheadend/gpg.key" \
    -o /tmp/tvh.key 2>/dev/null || true
if [ -s /tmp/tvh.key ]; then
    gpg --dearmor </tmp/tvh.key >/etc/apt/keyrings/tvheadend.gpg 2>/dev/null || true
fi
. /etc/os-release
if [ -s /etc/apt/keyrings/tvheadend.gpg ]; then
    # cloudsmith serves the CURRENT codename (trixie: verified HTTP 200); if a
    # future codename is not yet published, fall back to the last LTS rather
    # than to the Debian archive, which has not carried tvheadend for years —
    # "Unable to locate package tvheadend" from BOTH sources on smk-tv,
    # 2026-09-04, was the archive fallback chasing a package that is not there.
    _tvh_base="https://dl.cloudsmith.io/public/tvheadend/tvheadend/deb/${ID}"
    _tvh_code="$VERSION_CODENAME"
    if [ "$(curl -s -o /dev/null --max-time 10 -w '%{http_code}' "${_tvh_base}/dists/${_tvh_code}/Release")" != 200 ]; then
        app_warn "tvheadend has no ${_tvh_code} repo yet — falling back to bookworm"
        _tvh_code=bookworm
    fi
    echo "deb [signed-by=/etc/apt/keyrings/tvheadend.gpg] ${_tvh_base} ${_tvh_code} main" \
        >/etc/apt/sources.list.d/tvheadend.list
    apt-get update -qq >/dev/null 2>&1 || true
fi
rm -f /tmp/tvh.key

export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq tvheadend >/var/log/appliance-tvheadend.log 2>&1 ||
    app_die "tvheadend did not install from the project repo — see /var/log/appliance-tvheadend.log"
dpkg -s tvheadend >/dev/null 2>&1 || app_die "apt returned 0 but tvheadend is not installed"

getent passwd hts >/dev/null || app_die "the hts user was not created by the package"
chown -R hts:hts /srv/recordings /srv/timeshift /var/lib/tvheadend
usermod -aG video hts 2>/dev/null || true

install -d -m 0755 /etc/systemd/system/tvheadend.service.d
cat >/etc/systemd/system/tvheadend.service.d/10-kldload.conf <<CONF
[Unit]
After=zfs-mount.service
RequiresMountsFor=/srv/recordings /var/lib/tvheadend

[Service]
Restart=on-failure
RestartSec=5
CONF

systemctl daemon-reload
app_firewall tvheadend "$TVH_ALLOW_CIDR" 9981/tcp 9982/tcp
app_enable tvheadend

app_log "waiting for tvheadend on :9981"
app_wait_http http://127.0.0.1:9981/ 90 ||
    app_warn "not answering — journalctl -u tvheadend -n 50"

echo
app_check "tvheadend answers on :9981"  app_wait_http http://127.0.0.1:9981/ 15
app_check "tvheadend enabled"           systemctl is-enabled tvheadend
app_check "recordings dataset mounted"  mountpoint -q /srv/recordings
_tuners=0
for _a in /dev/dvb/adapter*; do [ -e "$_a" ] && _tuners=$((_tuners + 1)); done
if [ "${_tuners:-0}" -ge 1 ]; then
    app_check "tuner(s) visible (${_tuners})" true
else
    app_warn "no /dev/dvb adapters in the guest — tuner passthrough missed or nothing plugged in"
fi
[ -n "${APP_POOL:-}" ] && app_snapshot postinstall-tvheadend

cat <<EOM

  Tvheadend DVR

  Web UI      http://$(hostname -I 2>/dev/null | awk '{print $1}'):9981/
  Tuners      /dev/dvb: ${_tuners:-0} adapter(s)
  Recordings  /srv/recordings (quota ${TVH_REC_QUOTA})
  Streams     http://<vm-ip>:9981/playlist  (open in VLC)

EOM
app_summary
`
