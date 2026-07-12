#!/usr/bin/env bash
# Coverage gate (AGENTS.md requires each Go module to meet its threshold independently).
# Usage: coverage-gate.sh <coverprofile> <threshold>
#
# A module with no coverable statements passes as an empty set. The gate measures
# implemented statements; it does not require artificial tests for doc-only packages.
set -euo pipefail

profile="$1"
threshold="$2"

if [ ! -f "$profile" ]; then
  echo "coverage profile does not exist: ${profile}" >&2
  exit 1
fi

statements=$(grep -c -v '^mode:' "$profile" || true)
if [ "$statements" -eq 0 ]; then
  echo "coverage: module has no coverable statements; threshold ${threshold}% passes"
  exit 0
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ { sub(/%/, "", $NF); print $NF }')
echo "coverage total: ${total}% (threshold ${threshold}%)"
awk -v t="$total" -v th="$threshold" 'BEGIN { exit (t + 0 >= th + 0) ? 0 : 1 }' || {
  echo "coverage ${total}% is below threshold ${threshold}%" >&2
  exit 1
}
