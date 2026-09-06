# postinstall-example.sh — a moderately complex post-install to paste into
# vmxplore's New VM / EZ Fleet "post-install" box, or run by hand as root.
#
# What it does, in order:
#   1. tells the family (apt or dnf) and installs nginx, jq and curl;
#   2. creates an unprivileged user `app` and a status page under /srv/app,
#      rewritten every 30 s by a systemd timer: hostname, address, uptime,
#      load, disk, kernel — and a /healthz JSON the same timer refreshes;
#   3. points nginx at it, opens http in firewalld where firewalld exists;
#   4. writes a login banner (MOTD) with the same facts;
#   5. leaves a marker so a second run is a check, not a reinstall;
#   6. verifies every one of the above and prints a summary; exits non-zero
#      if any check failed, so the failure is visible in
#      /var/log/cloud-init-output.log rather than a page that never comes.
#
# WHY this shape: it exercises everything a real post-install needs — a
# package transaction on either family, a unit + timer, a user, a config
# file, a firewall rule, idempotency, and a verdict — without depending on
# anything kldload-specific, so it runs on a stock cloud image too.
#
# HOW it is run when pasted: vmxplore wraps it in `#!/usr/bin/env bash` +
# `set -Eeuo pipefail`, writes it to /var/lib/vmxplore-postinstall.sh and
# runs it once as root from cloud-init's runcmd. Output lands in
# /var/log/cloud-init-output.log. Because of `set -e`, every command that
# may legitimately fail says why in a comment beside its `|| true`.
#
# Inputs:  none required. Optional env: APP_TITLE (page heading).
# Outputs: /srv/app/index.html, /srv/app/healthz.json, guest-status.{service,timer},
#          /etc/nginx/conf.d/app.conf (dnf) or sites-enabled/app (apt),
#          /etc/motd, /var/lib/postinstall-example.done
# Exit:    0 all checks passed · 1 one or more checks failed · 2 not root

log() { printf '[%(%F %T)T] [postinstall] %s\n' -1 "$*"; }
trap 'log "FAIL at line $LINENO: $BASH_COMMAND"' ERR
# `hostname`, `sudo`, `runuser`, even `uptime` are missing from one base
# image or another (found the hard way in debian:trixie-slim and fedora:44
# containers, 2026-09-06), so the script leans on what is always there.
host_name() { uname -n; }
# run_as_app CMD… — as the app user, with whichever tool this image has:
# runuser (util-linux), setpriv (util-linux-core), su (always). Only the
# no-systemd path needs it; with systemd the unit runs as User=app.
run_as_app() {
    if command -v runuser >/dev/null 2>&1; then runuser -u app -- "$@"
    elif command -v setpriv >/dev/null 2>&1; then setpriv --reuid=app --regid=app --init-groups "$@"
    elif command -v su >/dev/null 2>&1; then su -s /bin/bash app -c "$(printf '%q ' "$@")"
    else
        # fedora:44's container base has none of the three; the render is a
        # one-off here, so do it as root and hand the files to app after
        log "no runuser/setpriv/su on this image — rendering as root, files handed to app"
        "$@" && chown -R app:app /srv/app
    fi
}
first_ip() {
    hostname -I 2>/dev/null | awk '{print $1; exit}' ||
        ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") {print $(i+1); exit}}' || true
}

[[ $(id -u) -eq 0 ]] || { log "must run as root"; exit 2; }
MARK=/var/lib/postinstall-example.done
APP_TITLE="${APP_TITLE:-guest status}"
if [[ -f $MARK ]]; then
    log "already ran on $(cat "$MARK") — re-verifying only"
    RERUN=1
else
    RERUN=0
fi

# ── 1. family + packages ────────────────────────────────────────────────────
# The family decides the package manager AND where nginx wants its vhost:
# Debian's nginx reads sites-enabled/, Fedora's reads conf.d/. Getting that
# wrong is the classic "installed fine, serves the default page" failure.
if command -v apt-get >/dev/null 2>&1; then
    FAMILY=deb; VHOST=/etc/nginx/sites-enabled/app
elif command -v dnf >/dev/null 2>&1; then
    FAMILY=rpm; VHOST=/etc/nginx/conf.d/app.conf
else
    log "neither apt-get nor dnf on this image — cannot install packages"; exit 1
fi
log "family: $FAMILY"
if [[ $RERUN -eq 0 ]]; then
    if [[ $FAMILY == deb ]]; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq >/dev/null
        apt-get install -y -qq --no-install-recommends nginx jq curl >/dev/null
    else
        dnf -y -q install nginx jq curl >/dev/null
    fi
fi
# the outcome, not the exit code: every binary must actually be on PATH
for b in nginx jq curl; do
    command -v "$b" >/dev/null 2>&1 || { log "package transaction returned but $b is missing"; exit 1; }
done
log "packages present: nginx jq curl"

# ── 2. the app user and the status generator ───────────────────────────────
id app >/dev/null 2>&1 || useradd -r -m -d /srv/app -s /usr/sbin/nologin app
install -d -m 0755 -o app -g app /srv/app
# The generator is the whole "application": a shell script that renders the
# page and the health JSON from live facts. It runs as `app`, so it can only
# ever write its own directory.
cat >/usr/local/bin/guest-status <<'GEN'
#!/usr/bin/env bash
set -Eeuo pipefail
out=/srv/app
host=$(uname -n)
ip=$(hostname -I 2>/dev/null | awk '{print $1; exit}' || ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") {print $(i+1); exit}}' || true)
ip=${ip:-no address}
# /proc/uptime, not `uptime -p`: procps is not on every image (debian slim
# has no uptime binary at all, and set -e turned that into exit 127)
up=$(awk '{s=int($1); printf "%dd %dh %dm", s/86400, (s%86400)/3600, (s%3600)/60}' /proc/uptime)
load=$(cut -d' ' -f1-3 /proc/loadavg)
disk=$(df -h / | awk 'NR==2{print $3" used of "$2" ("$5")"}')
kern=$(uname -r)
now=$(date -Is)
title="${APP_TITLE:-guest status}"
tmp=$(mktemp -p "$out")
cat >"$tmp" <<HTML
<!doctype html><meta charset="utf-8"><title>$host</title>
<style>body{font:16px/1.5 system-ui;margin:3rem auto;max-width:42rem;color:#e8eaf0;background:#0e1014}
h1{font-weight:600}dl{display:grid;grid-template-columns:8rem 1fr;gap:.3rem 1rem}dt{color:#8b93a7}code{color:#c77dff}</style>
<h1>$title · <code>$host</code></h1>
<dl><dt>address</dt><dd>$ip</dd><dt>uptime</dt><dd>$up</dd><dt>load</dt><dd>$load</dd>
<dt>disk /</dt><dd>$disk</dd><dt>kernel</dt><dd>$kern</dd><dt>rendered</dt><dd>$now</dd></dl>
<p>Built by a pasted post-install: nginx, a user, a timer, a health check. <a href="/healthz">/healthz</a></p>
HTML
mv -f "$tmp" "$out/index.html"; chmod 0644 "$out/index.html"
tmp=$(mktemp -p "$out")
jq -n --arg host "$host" --arg ip "$ip" --arg kernel "$kern" --arg at "$now" --arg load "$load" \
    '{ok:true, host:$host, ip:$ip, kernel:$kernel, load:$load, rendered:$at}' >"$tmp"
mv -f "$tmp" "$out/healthz.json"; chmod 0644 "$out/healthz.json"
GEN
chmod 0755 /usr/local/bin/guest-status

cat >/etc/systemd/system/guest-status.service <<UNIT
[Unit]
Description=Render the guest status page and health JSON
[Service]
Type=oneshot
User=app
Environment=APP_TITLE=${APP_TITLE}
ExecStart=/usr/local/bin/guest-status
UNIT
cat >/etc/systemd/system/guest-status.timer <<'UNIT'
[Unit]
Description=Refresh the guest status page every 30 s
[Timer]
OnBootSec=10s
OnUnitActiveSec=30s
AccuracySec=5s
[Install]
WantedBy=timers.target
UNIT

# ── 3. nginx in front ───────────────────────────────────────────────────────
cat >"$VHOST" <<NGX
server {
    listen 80 default_server;
    server_name _;
    root /srv/app;
    index index.html;
    location = /healthz { default_type application/json; alias /srv/app/healthz.json; }
    location / { try_files \$uri \$uri/ =404; }
}
NGX
# Debian ships a default site that also claims default_server on :80;
# two default_servers is a config error, so it goes.
[[ $FAMILY == deb ]] && rm -f /etc/nginx/sites-enabled/default
# nginx must be able to read a directory owned by app (0755 above); on
# SELinux hosts the path also needs the httpd content label
if command -v restorecon >/dev/null 2>&1 && command -v semanage >/dev/null 2>&1; then
    semanage fcontext -a -t httpd_sys_content_t '/srv/app(/.*)?' 2>/dev/null || true # already defined on a rerun
    restorecon -R /srv/app
fi
nginx -t >/dev/null 2>&1 || { log "nginx refuses the config:"; nginx -t; exit 1; }

# ── 4. login banner ─────────────────────────────────────────────────────────
printf 'This is %s (%s) — built by a pasted post-install. Status page: http://%s/\n' \
    "$(host_name)" "$(first_ip)" "$(first_ip)" >/etc/motd

# ── 5. start everything — where there is a systemd to start it with ─────────
# Inside a container (no systemd) the files are still laid down and checked;
# the units simply cannot be started, and the checks below say so.
HAVE_SYSTEMD=0
[[ -d /run/systemd/system ]] && HAVE_SYSTEMD=1
if [[ $HAVE_SYSTEMD -eq 1 ]]; then
    systemctl daemon-reload
    systemctl enable --now guest-status.timer >/dev/null
    systemctl start guest-status.service # render once now, not in 10 s
    systemctl enable --now nginx >/dev/null
    systemctl reload nginx
    if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active firewalld >/dev/null 2>&1; then
        firewall-cmd -q --permanent --add-service=http
        firewall-cmd -q --reload
    fi
else
    log "no systemd in this environment — rendering once by hand, units not started"
    run_as_app env APP_TITLE="$APP_TITLE" /usr/local/bin/guest-status
fi
date -Is >"$MARK"

# ── 6. verify — the outcome, never the exit code ────────────────────────────
pass=0; fail=0
check() {
    local label=$1; shift
    if "$@" >/dev/null 2>&1; then printf '  %-44s OK\n' "$label"; pass=$((pass + 1))
    else printf '  %-44s FAIL\n' "$label"; fail=$((fail + 1)); fi
}
# A service that was just reloaded answers a moment later than the reload
# returns: on Debian the package starts nginx with its default site, and the
# first request after our reload still got that page (pitest-1, 2026-09-06).
# Ask up to ten times, a second apart, before calling it a failure.
serves() { for _ in 1 2 3 4 5 6 7 8 9 10; do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
echo
check "app user exists"                    id app
check "status page rendered"               test -s /srv/app/index.html
check "health JSON is valid and ok:true"   bash -c 'jq -e ".ok == true" /srv/app/healthz.json'
check "page carries this hostname"         grep -q "$(host_name)" /srv/app/index.html
check "nginx config valid"                 nginx -t
check "MOTD written"                       grep -q "post-install" /etc/motd
check "marker written"                     test -s "$MARK"
if [[ $HAVE_SYSTEMD -eq 1 ]]; then
    check "guest-status.timer enabled"     systemctl is-enabled guest-status.timer
    check "guest-status.timer active"      systemctl is-active guest-status.timer
    check "nginx enabled"                  systemctl is-enabled nginx
    check "nginx serves the page"          serves bash -c 'curl -fsS http://127.0.0.1/ | grep -q "$(uname -n)"'
    check "/healthz answers ok:true"       serves bash -c 'curl -fsS http://127.0.0.1/healthz | jq -e ".ok == true"'
fi
echo
if [[ $fail -eq 0 ]]; then
    log "RESULT: VERIFIED — $pass/$pass checks passed · http://$(first_ip)/"
else
    log "RESULT: INCOMPLETE — $fail of $((pass + fail)) checks failed (see above)"
    exit 1
fi
