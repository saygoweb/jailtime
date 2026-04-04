#!/bin/bash
# iptables-ensure.sh — insert an iptables rule only if it is not already present.
#
# Uses `iptables -C` (check) before `iptables -I` (insert) to make the
# operation idempotent.  All arguments are forwarded verbatim; only the
# leading -I / -A flag is replaced with -C for the check.
#
# Usage: iptables-ensure.sh [-t table] -I|-A <chain> [rule-spec...]
#
# Examples:
#   iptables-ensure.sh -I INPUT -p tcp -m multiport --dports 80,443 \
#       -m set --match-set jt_nginx src -j DROP
#
#   iptables-ensure.sh -t nat -I PREROUTING -p tcp --dport 443 \
#       -m set --match-set jt_webapp src -j REDIRECT --to-port 8081

set -euo pipefail

args=("$@")
check_args=()
replaced=0

for arg in "${args[@]}"; do
    if [[ "$replaced" -eq 0 && ( "$arg" == "-I" || "$arg" == "-A" ) ]]; then
        check_args+=("-C")
        replaced=1
    else
        check_args+=("$arg")
    fi
done

if [[ "$replaced" -eq 0 ]]; then
    echo "iptables-ensure: no -I or -A flag found in arguments" >&2
    exit 1
fi

if iptables "${check_args[@]}" &>/dev/null; then
    echo "iptables-ensure: rule already exists, skipping insert" >&2
    exit 0
fi

iptables "${args[@]}"
echo "iptables-ensure: rule inserted" >&2
