#!/bin/bash
# iptables-remove.sh — delete an iptables rule only if it currently exists.
#
# Uses `iptables -C` (check) before `iptables -D` (delete) to make the
# operation idempotent.  All arguments are forwarded verbatim; only the
# leading -D flag is replaced with -C for the check.
#
# Usage: iptables-remove.sh [-t table] -D <chain> [rule-spec...]
#
# Examples:
#   iptables-remove.sh -D INPUT -p tcp -m multiport --dports 80,443 \
#       -m set --match-set jt_nginx src -j DROP
#
#   iptables-remove.sh -t nat -D PREROUTING -p tcp --dport 443 \
#       -m set --match-set jt_webapp src -j REDIRECT --to-port 8081

set -euo pipefail

args=("$@")
check_args=()
replaced=0

for arg in "${args[@]}"; do
    if [[ "$replaced" -eq 0 && "$arg" == "-D" ]]; then
        check_args+=("-C")
        replaced=1
    else
        check_args+=("$arg")
    fi
done

if [[ "$replaced" -eq 0 ]]; then
    echo "iptables-remove: no -D flag found in arguments" >&2
    exit 1
fi

if ! iptables "${check_args[@]}" &>/dev/null; then
    echo "iptables-remove: rule does not exist, nothing to remove" >&2
    exit 0
fi

iptables "${args[@]}"
echo "iptables-remove: rule removed" >&2
