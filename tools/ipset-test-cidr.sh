#!/bin/bash
# ipset-test-cidr.sh — Test whether an IP's CIDR block is already in a hash:net
# ipset.  Used as the jailtime 'query' pre-check to avoid duplicate actions.
#
# Usage: ipset-test-cidr.sh <setname> <ip>
#
# Exit codes:
#   0  — CIDR is present in the set (IP is already banned)
#   1  — CIDR is not present, or iptocidr failed, or output was invalid
#
# Depends on: iptocidr (must be on PATH), ipset

set -uo pipefail

SETNAME="${1:?ipset-test-cidr.sh: missing argument 1 (setname)}"
IP="${2:?ipset-test-cidr.sh: missing argument 2 (ip)}"

# ── Resolve IP → CIDR ─────────────────────────────────────────────────────────
command -v iptocidr &>/dev/null || exit 1

CIDR=$(iptocidr "$IP" 2>/dev/null) || exit 1

# ── Validate output ───────────────────────────────────────────────────────────
echo "$CIDR" | grep -qE '^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$' || exit 1

PREFIX="${CIDR##*/}"
[[ "$PREFIX" -ge 0 && "$PREFIX" -le 32 ]] || exit 1

# ── Test membership ───────────────────────────────────────────────────────────
ipset test "$SETNAME" "$CIDR" 2>/dev/null
