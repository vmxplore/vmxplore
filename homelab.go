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
	Name:     "Jellyfin",
	Summary:  "Free software media server — your films and shows, no account, no cloud",
	Homepage: "https://jellyfin.org",
	License:  "GPL-2.0",

	Distro: "debian",
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 20,

	Port:    8096,
	LandsOn: "http://<vm-ip>:8096/  (setup wizard on first visit)",

	Notes: "Installed from Jellyfin's own Debian repository, whose signing " +
		"key is checked against a pinned fingerprint before it is trusted.\n\n" +
		"The media directory is created empty and handed to the jellyfin " +
		"user. A 20 GB VM disk holds the server and its metadata, NOT a " +
		"library — point the media directory at a mount you attach " +
		"separately (a second disk, an NFS or SMB share) rather than growing " +
		"this VM's root. Add it in the wizard as a library once it has " +
		"content.\n\n" +
		"Hardware transcoding needs a GPU passed through to the VM and is " +
		"off by default; without it, transcoding is CPU-only and 4K will " +
		"struggle. Direct play is unaffected.",

	Fields: []ApplianceField{
		{Key: "JF_MEDIA_DIR", Label: "media directory",
			Placeholder: "where your library is mounted",
			Default:     "/srv/media", Required: true},
	},

	Validate: func(v map[string]string) error {
		return checkAbsDir(v["JF_MEDIA_DIR"], "media directory")
	},

	Script: jellyfinScript,
}

const jellyfinScript = `JF_KEY_URL='https://repo.jellyfin.org/jellyfin_team.gpg.key'
JF_KEY_FPR='4918AABC486CA052358D778D49023CD01DE21A7B'
JF_SUITE='trixie'

export DEBIAN_FRONTEND=noninteractive

# ─── Prerequisites ───────────────────────────────────────────────────
# gnupg for --dearmor, ca-certificates so the key fetch can be trusted at
# all. Both are absent from the Debian genericcloud image.
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg

# ─── Trust, but verify ───────────────────────────────────────────────
# The fingerprint check is the entire point: without it this is "trust
# whatever answered the request", forever, for every package the VM
# installs from that repo.
install -d -m 0755 /etc/apt/keyrings
tmpkey="$(mktemp)"
trap 'rm -f "$tmpkey"' EXIT
curl -fsSL "$JF_KEY_URL" -o "$tmpkey"

got_fpr="$(gpg --show-keys --with-colons --with-fingerprint "$tmpkey" |
    awk -F: '$1 == "fpr" { print $10; exit }')"
if [[ "$got_fpr" != "$JF_KEY_FPR" ]]; then
    echo "FATAL: Jellyfin signing key fingerprint mismatch" >&2
    echo "  expected $JF_KEY_FPR" >&2
    echo "  got      ${got_fpr:-<none>}" >&2
    exit 1
fi
gpg --dearmor --yes -o /etc/apt/keyrings/jellyfin.gpg "$tmpkey"
chmod 0644 /etc/apt/keyrings/jellyfin.gpg

# arch= is pinned so apt does not ask the repo for indexes it has no
# packages for, which shows up as a confusing 404 during every update.
deb_arch="$(dpkg --print-architecture)"
cat >/etc/apt/sources.list.d/jellyfin.list <<EOF
deb [arch=${deb_arch} signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/debian ${JF_SUITE} main
EOF

# ─── Install ─────────────────────────────────────────────────────────
apt-get update
apt-get install -y jellyfin

# ─── The library location ────────────────────────────────────────────
# Created empty and owned by jellyfin so the wizard can actually add it.
# If the operator later mounts a share here, the mount's own ownership
# wins — this only guarantees the path exists and is usable today.
install -d -m 0775 -o jellyfin -g jellyfin "$JF_MEDIA_DIR"

systemctl enable --now jellyfin

# ─── Prove it ────────────────────────────────────────────────────────
# The service unit returning 0 says systemd started a process, not that
# the server is serving. Poll the port so a broken install fails here,
# where the log is being read, rather than at the operator's browser.
for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "http://127.0.0.1:8096/System/Ping" 2>/dev/null ||
        curl -fsS -o /dev/null "http://127.0.0.1:8096/" 2>/dev/null; then
        echo "jellyfin: responding on 8096"
        exit 0
    fi
    sleep 5
done
echo "FATAL: jellyfin did not answer on 8096 after 5 minutes" >&2
systemctl --no-pager --full status jellyfin >&2 || true
exit 1
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

// ─── Git: Gitea ──────────────────────────────────────────────────────
//
// A pinned static binary with per-arch checksums rather than a repo, because
// that is what upstream actually ships. SQLite is the database on purpose:
// a lab's git server is not a Postgres tenant, and one file is a backup
// story an operator can carry out on a USB stick.

var gitea = Appliance{
	Name:     "Gitea",
	Summary:  "Self-hosted git service — repos, issues, CI runners, in one binary",
	Homepage: "https://about.gitea.com",
	License:  "MIT",

	Distro: "debian",
	VCPUs:  1,
	RAMMB:  1024,
	DiskGB: 20,

	Port:    3000,
	LandsOn: "http://<vm-ip>:3000/  (git over ssh on port 2222)",

	Notes: "A single pinned binary, verified against upstream's published " +
		"SHA-256 for this architecture, with SQLite as the database — the " +
		"whole service backs up by copying /var/lib/gitea.\n\n" +
		"Gitea's built-in SSH server listens on 2222 so it does not fight " +
		"the VM's own sshd. Clone URLs therefore look like " +
		"ssh://git@<vm-ip>:2222/user/repo.git.\n\n" +
		"The install wizard is locked and the admin account is created " +
		"during the build, so the service comes up ready to log into. " +
		"Open registration is DISABLED — add users from the admin panel. " +
		"Turn it on in /etc/gitea/app.ini if this is a lab where that is " +
		"wanted.",

	Fields: []ApplianceField{
		{Key: "GITEA_ADMIN_USER", Label: "admin username",
			Default: "gitadmin", Required: true},
		{Key: "GITEA_ADMIN_PASS", Label: "admin password",
			Placeholder: "blank = generate one", Secret: true,
			Generate: true, Required: true},
		{Key: "GITEA_ADMIN_EMAIL", Label: "admin email",
			Placeholder: "you@example.com",
			// Gitea stores this and uses it for notifications; a bare
			// "user@localhost" is not deliverable and not a valid address
			// by the check below, so the default is an obvious placeholder
			// the operator is meant to replace.
			Default: "gitadmin@example.com", Required: true},
	},

	Validate: func(v map[string]string) error {
		// Gitea rejects these itself, deep inside the guest's first boot
		// where nobody is watching the log. Fail on the form instead.
		if u := v["GITEA_ADMIN_USER"]; len(u) < 3 {
			return fmt.Errorf("admin username %q must be at least 3 characters", u)
		}
		if len(v["GITEA_ADMIN_PASS"]) < 8 {
			return fmt.Errorf("admin password must be at least 8 characters")
		}
		if e := v["GITEA_ADMIN_EMAIL"]; !applianceEmailRE.MatchString(e) {
			return fmt.Errorf("admin email %q does not look like an address", e)
		}
		return nil
	},

	Script: giteaScript,
}

const giteaScript = `GITEA_VERSION='1.27.2'
GITEA_SHA256_amd64='aa4e624ca6aa58a824a75562caecc2d206fcab8c70bc8fab765b456f182844fd'
GITEA_SHA256_arm64='a585d7ce94bacb81241ec39b0e3dc99b173c9d7dd41cd3e5c28445a30271c3ab'

export DEBIAN_FRONTEND=noninteractive

# The binary is published per-arch. Anything else is a hard stop rather
# than an exec-format error at first start, which reads as "broken".
case "$(uname -m)" in
    x86_64)
        g_arch=amd64
        g_sha="$GITEA_SHA256_amd64"
        ;;
    aarch64 | arm64)
        g_arch=arm64
        g_sha="$GITEA_SHA256_arm64"
        ;;
    *)
        echo "FATAL: no Gitea build for $(uname -m)" >&2
        exit 1
        ;;
esac

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git

# ─── Fetch and verify ────────────────────────────────────────────────
tmpd="$(mktemp -d)"
trap 'rm -rf "$tmpd"' EXIT
curl -fsSL -o "$tmpd/gitea" \
    "https://dl.gitea.com/gitea/${GITEA_VERSION}/gitea-${GITEA_VERSION}-linux-${g_arch}"
echo "${g_sha}  ${tmpd}/gitea" | sha256sum -c -

install -m 0755 -o root -g root "$tmpd/gitea" /usr/local/bin/gitea

# ─── Layout ──────────────────────────────────────────────────────────
# Upstream's documented layout: config root-owned and group-readable by
# git, data git-owned. Gitea refuses to start if app.ini is writable by
# the service user once the install is locked.
adduser --system --group --disabled-password --shell /bin/bash \
    --home /home/git --gecos 'Gitea' git
install -d -m 0750 -o git -g git /var/lib/gitea/custom /var/lib/gitea/data /var/lib/gitea/log
install -d -m 0770 -o root -g git /etc/gitea

# ─── Config ──────────────────────────────────────────────────────────
# INSTALL_LOCK skips the web installer, which otherwise leaves the
# service wide open to whoever reaches it first. The secrets are minted
# here rather than left for Gitea to invent at first start, because that
# path needs app.ini writable by the service user.
secret_key="$(gitea generate secret SECRET_KEY)"
internal_token="$(gitea generate secret INTERNAL_TOKEN)"

cat >/etc/gitea/app.ini <<EOF
APP_NAME = Gitea
RUN_USER = git
RUN_MODE = prod
WORK_PATH = /var/lib/gitea

[server]
PROTOCOL         = http
HTTP_ADDR        = 0.0.0.0
HTTP_PORT        = 3000
APP_DATA_PATH    = /var/lib/gitea/data
DISABLE_SSH      = false
START_SSH_SERVER = true
SSH_PORT         = 2222
SSH_LISTEN_PORT  = 2222
LFS_START_SERVER = true

[database]
DB_TYPE = sqlite3
PATH    = /var/lib/gitea/data/gitea.db

[security]
INSTALL_LOCK   = true
SECRET_KEY     = ${secret_key}
INTERNAL_TOKEN = ${internal_token}

[service]
DISABLE_REGISTRATION = true

[repository]
ROOT = /var/lib/gitea/data/gitea-repositories

[log]
ROOT_PATH = /var/lib/gitea/log
MODE      = console
LEVEL     = info
EOF
chown root:git /etc/gitea/app.ini
chmod 0640 /etc/gitea/app.ini

# ─── Schema and the admin account ────────────────────────────────────
# Both run as git so nothing under /var/lib/gitea ends up root-owned —
# the classic cause of a Gitea that starts, then cannot write.
su - git -s /bin/bash -c \
    "GITEA_WORK_DIR=/var/lib/gitea /usr/local/bin/gitea migrate -c /etc/gitea/app.ini"
su - git -s /bin/bash -c \
    "GITEA_WORK_DIR=/var/lib/gitea /usr/local/bin/gitea admin user create \
        --admin --username $(printf '%q' "$GITEA_ADMIN_USER") \
        --password $(printf '%q' "$GITEA_ADMIN_PASS") \
        --email $(printf '%q' "$GITEA_ADMIN_EMAIL") \
        --must-change-password=false -c /etc/gitea/app.ini"

# ─── Unit ────────────────────────────────────────────────────────────
cat >/etc/systemd/system/gitea.service <<'EOF'
[Unit]
Description=Gitea
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=git
Group=git
WorkingDirectory=/var/lib/gitea
ExecStart=/usr/local/bin/gitea web --config /etc/gitea/app.ini
Environment=USER=git HOME=/home/git GITEA_WORK_DIR=/var/lib/gitea
Restart=always
RestartSec=2s
# Gitea's built-in ssh server binds 2222, so no privileged port is needed.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now gitea

for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "http://127.0.0.1:3000/api/healthz" 2>/dev/null ||
        curl -fsS -o /dev/null "http://127.0.0.1:3000/" 2>/dev/null; then
        echo "gitea: responding on 3000"
        exit 0
    fi
    sleep 5
done
echo "FATAL: gitea did not answer on 3000 after 5 minutes" >&2
systemctl --no-pager --full status gitea >&2 || true
exit 1
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
		{Key: "ST_DATA_DIR", Label: "shared folder directory",
			Placeholder: "where synced folders live",
			Default:     "/srv/sync", Required: true},
	},

	Validate: func(v map[string]string) error {
		return checkAbsDir(v["ST_DATA_DIR"], "shared folder directory")
	},

	Script: syncthingScript,
}

const syncthingScript = `ST_KEY_URL='https://syncthing.net/release-key.gpg'
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
