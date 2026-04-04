#!/bin/bash
# ipset-flush.sh — flush entries from an ipset without destroying it.
#
# This allows the ipset (and any iptables rules referencing it) to
# survive a jailtime restart or reload.  Existing bans are cleared
# but the kernel data structure is kept so iptables rules stay valid.
#
# Usage: ipset-flush.sh <name>

set -euo pipefail

NAME="${1:?ipset-flush.sh: missing ipset name}"

if ! ipset list "$NAME" &>/dev/null; then
    echo "ipset-flush: '$NAME' does not exist, nothing to flush" >&2
    exit 0
fi

ipset flush "$NAME"
echo "ipset-flush: flushed '$NAME'" >&2
