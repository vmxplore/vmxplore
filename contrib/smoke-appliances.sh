#!/usr/bin/env bash
# smoke-appliances.sh — thin wrapper; the engine lives IN vmxplore now.
#
# The original shell implementation moved into the binary as `vmx --selftest`
# (selftest.go) so the tool that owns the catalog owns its proof — a script
# beside the binary is the two-divergent-copies trap this project already
# paid for once with wgx. Flags map 1:1: --keep, --only <tile|st-slug>.
set -Eeuo pipefail
exec sudo -n vmx --selftest "$@"
