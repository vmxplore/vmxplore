#!/usr/bin/env bash
#
# ─────────────────────────────────────────────────────────────────────────────
# smoke-appliances.sh — deploy every catalog tile for real, and prove it.
#
# WHAT IT DOES, IN ORDER, per appliance:
#   1. Builds it with the same CLI the operator uses:
#        vmx --appliance "<name>" --vm smk-<slug>
#      (defaults + generated secrets — the zero-typing path a user takes).
#   2. Verifies OUTCOMES, never exit codes alone:
#        - the recipe's own verdict inside the guest (RESULT: VERIFIED,
#          N/N checks) read from the cloud-init log
#        - the in-guest pool exists on the data disk (NeedsZFS tiles) —
#          the check that caught DataGB being written-but-not-wired
#        - the service answers on its port inside the guest
#        - the management mesh (ap-<vm>) has a LIVE handshake on the host
#        - the estate cert is staged at /etc/kldload/tls/ in the guest
#        - the VM is registered in the state DB
#   3. Tears the VM down — unless it FAILED (kept for diagnosis) or --keep.
#
# WHY: ten tiles that each render to valid bash is a parser's opinion. This
# is the operator's: click the tile, get the appliance, on the substrate.
#
# WHERE IT RUNS: on a kldload host (fiend), as a sudo-capable user. Needs
# vmx >= 0.4.0 b14 (data disk wiring), kvm-mesh, kldload-ca.
#
# Usage:
#   smoke-appliances.sh                  # every tile, sequential
#   smoke-appliances.sh --only smk-web   # one tile by vm slug
#   smoke-appliances.sh --keep           # leave everything running
#   smoke-appliances.sh --list           # show the matrix and exit
#
# Exit: number of failed appliances (0 = all verified).
# ─────────────────────────────────────────────────────────────────────────────
set -Eeuo pipefail
trap 'echo "FAIL at line $LINENO: $BASH_COMMAND" >&2' ERR

G=$'\033[1;32m' R=$'\033[1;31m' Y=$'\033[1;33m' C=$'\033[1;36m' N=$'\033[0m'
KEEP=0 ONLY="" LIST=0
while [[ $# -gt 0 ]]; do
    case "$1" in
    --keep) KEEP=1 ;;
    --only)
        ONLY="${2:?--only needs a vm slug}"
        shift
        ;;
    --list) LIST=1 ;;
    *)
        echo "unknown flag: $1" >&2
        exit 2
        ;;
    esac
    shift
done

# ── the matrix ───────────────────────────────────────────────────────────────
# slug | catalog name | guest port | health path | zfs pool expected | verdict expected
# verdict=no marks the pre-substrate recipes that end without app_summary;
# they still get every other check.
APPS=(
    "smk-web|Web Stack|80|/healthz|yes|yes"
    "smk-ice|Icecast Stations|8001|/status.xsl|yes|yes"
    "smk-jelly|Jellyfin on ZFS|8096|/System/Info/Public|yes|yes"
    "smk-plex|Plex on ZFS|32400|/identity|yes|yes"
    "smk-seed|Seedbox|8080|/|yes|yes"
    "smk-sdr|SDR Station|8073|/|yes|yes"
    "smk-tvh|Tvheadend DVR|9981|/|yes|yes"
    "smk-gitea|Gitea|3000|/|yes|no"
    "smk-agh|AdGuard Home|3000|/|no|no"
    "smk-sync|Syncthing|8384|/|yes|no"
    "smk-wfd|WriteFreely Desktop|80|/|no|no"
)

if [[ $LIST -eq 1 ]]; then
    printf '  %-10s %-22s %-6s %s\n' SLUG TILE PORT "POOL/VERDICT"
    for row in "${APPS[@]}"; do
        IFS='|' read -r slug name port _ zfs verdict <<<"$row"
        printf '  %-10s %-22s %-6s %s/%s\n' "$slug" "$name" "$port" "$zfs" "$verdict"
    done
    exit 0
fi

command -v vmx >/dev/null || {
    echo "vmx not on PATH" >&2
    exit 2
}
command -v kvm-mesh >/dev/null || echo "${Y}NOTE: no kvm-mesh — enrollment checks will be skipped${N}"

# guest_ssh <ip> <cmd...> — root over the enrollment-seeded key. Guests are
# clones; no host-key pinning, no known_hosts pollution.
guest_ssh() {
    local ip="$1"
    shift
    sudo -n ssh -o BatchMode=yes -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null -o GlobalKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=8 "root@${ip}" "$@"
}

guest_ip() { # <vm> — agent first (non-loopback), lease as fallback
    local vm="$1" ip
    ip="$(sudo -n virsh domifaddr "$vm" --source agent 2>/dev/null |
        awk '/ipv4/ && $4 !~ /^127\./ {split($4,a,"/"); print a[1]; exit}')"
    [[ -n "$ip" ]] || ip="$(sudo -n virsh domifaddr "$vm" 2>/dev/null |
        awk '/ipv4/ {split($4,a,"/"); print a[1]; exit}')"
    printf '%s' "$ip"
}

teardown() { # <vm> — reverse order of creation; every step tolerant of absence
    local vm="$1"
    sudo -n virsh destroy "$vm" >/dev/null 2>&1 || true
    sudo -n virsh undefine "$vm" --nvram >/dev/null 2>&1 || true
    # the root and data zvols, wherever the parent was
    local ds
    while read -r ds; do
        [[ -n "$ds" ]] && sudo -n zfs destroy -r "$ds" >/dev/null 2>&1 || true
        # || true: a clean host has nothing matching, grep exits 1, and the ERR
        # trap narrates a non-event into the log. Empty is the desired state.
    done < <(sudo -n zfs list -H -o name 2>/dev/null | grep -E "/${vm}(-data)?$" || true)
    sudo -n rm -f "/var/lib/libvirt/images/${vm}.qcow2" \
        "/var/lib/libvirt/images/${vm}-data.qcow2" \
        "/var/lib/libvirt/images/${vm}-seed.iso"
    command -v kvm-mesh >/dev/null && sudo -n kvm-mesh down "ap-${vm}" >/dev/null 2>&1 || true
    # the state-DB row retires itself: kldload-networks sync pass 4 drops
    # VM rows whose domain no longer exists
    command -v kldload-networks >/dev/null && sudo -n kldload-networks sync >/dev/null 2>&1 || true
}

declare -A RESULT DETAIL
TOTAL=0 FAILED=0

for row in "${APPS[@]}"; do
    IFS='|' read -r slug name port path zfs verdict <<<"$row"
    [[ -n "$ONLY" && "$ONLY" != "$slug" ]] && continue
    TOTAL=$((TOTAL + 1))
    echo
    echo "${C}══ ${name} → ${slug} ══${N}"
    pass=0 fail=0
    ck() { # <label> <cmd...>
        local label="$1"
        shift
        printf '  %-38s ' "$label"
        if "$@" >/dev/null 2>&1; then
            echo "${G}OK${N}"
            pass=$((pass + 1))
        else
            echo "${R}FAIL${N}"
            fail=$((fail + 1))
        fi
    }

    # a stale run of the same slug poisons every check below — clear it first
    teardown "$slug"

    log="$(mktemp)"
    if timeout 1800 sudo -n vmx --appliance "$name" --vm "$slug" >"$log" 2>&1; then
        ck "vmx build + port wait" true
    else
        ck "vmx build + port wait" false
        tail -5 "$log" | sed 's/^/      /'
    fi

    ip="$(guest_ip "$slug")"
    if [[ -z "$ip" ]]; then
        echo "  ${R}no guest address — remaining checks impossible${N}"
        RESULT[$slug]="${R}FAIL${N}"
        DETAIL[$slug]="no address"
        FAILED=$((FAILED + 1))
        cp "$log" "/tmp/smoke-${slug}.log"
        rm -f "$log"
        continue
    fi
    echo "  guest: $ip   (log: /tmp/smoke-${slug}.log)"
    cp "$log" "/tmp/smoke-${slug}.log"
    rm -f "$log"

    ck "root ssh (seeded ops key)" guest_ssh "$ip" true

    if [[ "$verdict" == yes ]]; then
        # cloud-init may still be writing when the port answers; give the
        # verdict line a moment rather than failing the race
        v=""
        for _ in 1 2 3 4 5 6; do
            v="$(guest_ssh "$ip" \
                "grep -hoE 'RESULT: (VERIFIED|INCOMPLETE)' /var/log/cloud-init-output.log 2>/dev/null | tail -1")" || true
            [[ -n "$v" ]] && break
            sleep 10
        done
        ck "recipe verdict is VERIFIED" [ "$v" = "RESULT: VERIFIED" ]
        [[ "$v" == "RESULT: INCOMPLETE" ]] &&
            guest_ssh "$ip" "grep -E ' FAIL$' /var/log/cloud-init-output.log | tail -5" 2>/dev/null | sed 's/^/      /'
        n="$(guest_ssh "$ip" \
            "grep -hoE '[0-9]+/[0-9]+ checks passed' /var/log/cloud-init-output.log 2>/dev/null | tail -1")" || true
        [[ -n "$n" ]] && echo "      guest checks: $n"
    fi

    if [[ "$zfs" == yes ]]; then
        ck "in-guest pool on the data disk" guest_ssh "$ip" "zpool list -H tank"
    fi

    ck "service answers on :${port}" guest_ssh "$ip" \
        "curl -fsS --max-time 8 http://127.0.0.1:${port}${path}"

    if command -v kvm-mesh >/dev/null; then
        mesh="ap-${slug}"
        live="$(sudo -n wg show "$mesh" latest-handshakes 2>/dev/null |
            awk -v n="$(date +%s)" '$2>0 && (n-$2)<180' | grep -c . || true)"
        ck "mesh ${mesh} live handshake" [ "${live:-0}" -ge 1 ]
        ck "estate cert staged in guest" guest_ssh "$ip" "test -f /etc/kldload/tls/server.crt"
    fi
    if command -v kldload-db >/dev/null; then
        ck "registered in the state DB" bash -c \
            "sudo -n kldload-db dump 2>/dev/null | grep -q '\"$slug\"'"
    fi

    echo "  ── ${pass} passed, ${fail} failed"
    if [[ $fail -eq 0 ]]; then
        RESULT[$slug]="${G}PASS${N}"
        DETAIL[$slug]="${pass} checks"
        if [[ $KEEP -eq 0 ]]; then
            teardown "$slug"
            echo "  (torn down)"
        fi
    else
        RESULT[$slug]="${R}FAIL${N}"
        DETAIL[$slug]="${fail} of $((pass + fail)) checks failed"
        FAILED=$((FAILED + 1))
        echo "  ${Y}kept for diagnosis: virsh console ${slug} · /tmp/smoke-${slug}.log${N}"
    fi
done

echo
echo "${C}══════════ appliance smoke summary ══════════${N}"
for row in "${APPS[@]}"; do
    IFS='|' read -r slug name _ <<<"$row"
    [[ -n "${RESULT[$slug]:-}" ]] || continue
    printf '  %-10s %-22s %b  %s\n' "$slug" "$name" "${RESULT[$slug]}" "${DETAIL[$slug]}"
done
echo "  ${TOTAL} tile(s), ${FAILED} failed"
exit "$FAILED"
