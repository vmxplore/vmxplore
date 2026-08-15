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
	Name:     "Plex Media Server",
	Summary:  "Plex media server — the widest client support; free to run, proprietary",
	Homepage: "https://www.plex.tv",
	License:  "Proprietary (free tier; Plex Pass optional)",

	Distro: "debian",
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 20,

	Port:    32400,
	LandsOn: "http://<vm-ip>:32400/web  (claim it from a browser on this LAN)",

	Notes: "Installed from Plex's own Debian repository, whose signing key is " +
		"checked against a pinned fingerprint before it is trusted.\n\n" +
		"CLAIMING: open http://<vm-ip>:32400/web from a browser on the SAME " +
		"network as the VM and sign in. Plex treats same-subnet access as " +
		"trusted; reaching it from elsewhere first shows a lock screen " +
		"instead of the setup wizard. That is Plex's behaviour, not a fault " +
		"in the install.\n\n" +
		"As with any media server, the 20 GB root disk is for the server and " +
		"its metadata — mount the library separately rather than growing " +
		"this VM.\n\n" +
		"Proprietary software: it will phone home to plex.tv, and an account " +
		"is required to use it. Jellyfin is the free-software option in this " +
		"catalog if that matters to you.",

	Fields: []ApplianceField{
		{Key: "PLEX_MEDIA_DIR", Label: "media directory",
			Placeholder: "where your library is mounted",
			Default:     "/srv/media", Required: true},
	},

	Validate: func(v map[string]string) error {
		return checkAbsDir(v["PLEX_MEDIA_DIR"], "media directory")
	},

	Script: plexScript,
}

const plexScript = `PLEX_KEY_URL='https://downloads.plex.tv/plex-keys/PlexSign.key'
PLEX_KEY_FPR='CD665CBA0E2F88B7373F7CB997203C7B3ADCA79D'

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg

# ─── Trust, but verify ───────────────────────────────────────────────
install -d -m 0755 /etc/apt/keyrings
tmpkey="$(mktemp)"
trap 'rm -f "$tmpkey"' EXIT
curl -fsSL "$PLEX_KEY_URL" -o "$tmpkey"

got_fpr="$(gpg --show-keys --with-colons --with-fingerprint "$tmpkey" |
    awk -F: '$1 == "fpr" { print $10; exit }')"
if [[ "$got_fpr" != "$PLEX_KEY_FPR" ]]; then
    echo "FATAL: Plex signing key fingerprint mismatch" >&2
    echo "  expected $PLEX_KEY_FPR" >&2
    echo "  got      ${got_fpr:-<none>}" >&2
    exit 1
fi
gpg --dearmor --yes -o /etc/apt/keyrings/plex.gpg "$tmpkey"
chmod 0644 /etc/apt/keyrings/plex.gpg

deb_arch="$(dpkg --print-architecture)"
cat >/etc/apt/sources.list.d/plexmediaserver.list <<EOF
deb [arch=${deb_arch} signed-by=/etc/apt/keyrings/plex.gpg] https://downloads.plex.tv/repo/deb public main
EOF

# ─── Install ─────────────────────────────────────────────────────────
# WARN: the package ships its own apt source and will overwrite the file
# written above on upgrade. That is upstream's design; the pinned keyring
# stays in place either way.
apt-get update
apt-get install -y plexmediaserver

install -d -m 0775 -o plex -g plex "$PLEX_MEDIA_DIR"

systemctl enable --now plexmediaserver

# ─── Prove it ────────────────────────────────────────────────────────
# An unclaimed server answers /identity without authentication, which
# makes it the honest readiness probe — /web would 401 and read as a
# failure on a perfectly good install.
for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "http://127.0.0.1:32400/identity" 2>/dev/null; then
        echo "plex: responding on 32400 — claim it from a browser on this LAN"
        exit 0
    fi
    sleep 5
done
echo "FATAL: plexmediaserver did not answer on 32400 after 5 minutes" >&2
systemctl --no-pager --full status plexmediaserver >&2 || true
exit 1
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
