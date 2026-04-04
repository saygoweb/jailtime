#!/bin/bash
# ipset-add-cidr.sh — Convert an IP to its CIDR block via iptocidr and add it
# to a hash:net ipset.
#
# Usage: ipset-add-cidr.sh <setname> <ip> <timeout_seconds>
#
# Steps:
#   1. Calls `iptocidr <ip>` to get the network CIDR for the given address.
#   2. Validates that the output looks like a CIDR (x.x.x.x/nn).
#      Exits non-zero with a message if not.
#   3. Adds the CIDR to the ipset with the given per-entry timeout.
#      Uses -exist so re-adding an already-present entry updates its timeout
#      rather than erroring.
#
# Depends on: iptocidr (must be on PATH), ipset

set -euo pipefail

SETNAME="${1:?ipset-add-cidr.sh: missing argument 1 (setname)}"
IP="${2:?ipset-add-cidr.sh: missing argument 2 (ip)}"
TIMEOUT="${3:?ipset-add-cidr.sh: missing argument 3 (timeout_seconds)}"

# ── Resolve IP → CIDR ─────────────────────────────────────────────────────────
if ! command -v iptocidr &>/dev/null; then
    echo "ipset-add-cidr: 'iptocidr' not found on PATH" >&2
    exit 1
fi

# iptocidr may transiently fail when a concurrent invocation is writing the
# WHOIS cache for the same IP.  Retry once after a short delay before giving up.
CIDR=$(iptocidr "$IP" 2>/dev/null) || {
    sleep 1
    CIDR=$(iptocidr "$IP" 2>/dev/null) || {
        echo "ipset-add-cidr: iptocidr failed for IP '$IP'" >&2
        exit 1
    }
}

# ── Validate output ───────────────────────────────────────────────────────────
# Expected format: x.x.x.x/nn  (e.g. 8.8.8.0/24)
if ! echo "$CIDR" | grep -qE '^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$'; then
    echo "ipset-add-cidr: iptocidr returned unexpected output '$CIDR' for IP '$IP' (expected CIDR)" >&2
    exit 1
fi

# Sanity-check prefix length is 0–32
PREFIX="${CIDR##*/}"
if [[ "$PREFIX" -lt 0 || "$PREFIX" -gt 32 ]]; then
    echo "ipset-add-cidr: prefix length $PREFIX is out of range in '$CIDR'" >&2
    exit 1
fi

# ── Add to ipset ──────────────────────────────────────────────────────────────
ipset add -exist "$SETNAME" "$CIDR" timeout "$TIMEOUT" comment "from $IP"
echo "ipset-add-cidr: added $CIDR (resolved from $IP) to '$SETNAME' timeout=${TIMEOUT}s" >&2
