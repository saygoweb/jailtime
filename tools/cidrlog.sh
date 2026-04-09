#!/usr/bin/env bash

set -euo pipefail

TAG="${1:-}"

if [[ -z "$TAG" ]]; then
    echo "Usage: $0 <tag>"
    exit 1
fi

# Ensure stdin is not empty
if [ -t 0 ]; then
    echo "Error: No input on stdin"
    exit 1
fi

# Process incoming CIDRs (comma+space separated or newline)
sed 's/, /\n/g' | while read -r cidr; do
    [[ -z "$cidr" ]] && continue
    printf "%s %s %s\n" "$(date '+%Y-%m-%d %H:%M:%S')" "$TAG" "$cidr"
done
