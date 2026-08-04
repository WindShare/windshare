#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
vector_root='core/testvectors'
echo "== vectors =="

echo "-- regenerate canonical vector families"
go -C core test -count=1 ./internal/protocolcontract -update
go test -count=1 ./connectivity/v2signal -update

echo "-- compare vector inventory"
expected_inventory="$(grep -Ev '^[[:space:]]*(#|$)' "$vector_root/inventory.txt" | sort)"
actual_inventory="$(for file in "$vector_root"/*.json; do basename "$file"; done | sort)"
if [[ "$expected_inventory" != "$actual_inventory" ]]; then
  diff -u \
    <(printf '%s\n' "$expected_inventory") \
    <(printf '%s\n' "$actual_inventory") || true
  echo "$vector_root/inventory.txt does not match the JSON vector inventory" >&2
  exit 1
fi

echo "-- compare regenerated vectors with the worktree"
status="$(git -c core.quotepath=false status --short -- "$vector_root")"
if [[ -n "$status" ]]; then
  echo "regenerated vectors differ from committed $vector_root/:" >&2
  printf '%s\n' "$status" >&2
  exit 1
fi

echo "== vectors: PASS in ${SECONDS}s =="
