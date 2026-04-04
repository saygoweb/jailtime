#!/bin/bash
# ipset-ensure.sh — create an ipset only if it does not already exist.
#
# Usage: ipset-ensure.sh <name> <type> [ipset-create-options...]
#
# Example:
#   ipset-ensure.sh jt_nginx hash:ip timeout 0 comment
#
# Exits 0 whether the set was created or already existed.

set -euo pipefail

NAME="${1:?ipset-ensure.sh: missing ipset name}"
shift

if ipset list "$NAME" &>/dev/null; then
    echo "ipset-ensure: '$NAME' already exists, skipping create" >&2
    exit 0
fi

ipset create "$NAME" "$@"
echo "ipset-ensure: created '$NAME'" >&2
