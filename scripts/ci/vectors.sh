#!/usr/bin/env bash
# CI-parity golden-vector idempotence gate (Linux). Mirrors ci.yml
# golden-vectors / work-plan §10.3: regenerate every generated vector family
# twice; the regenerations must be byte-identical and must exactly match the
# committed core/testvectors/. Hashes stage in a mktemp dir (CI uses RUNNER_TEMP).
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== vectors =="
vector_root="core/testvectors"

hash_dir="$(mktemp -d)"
trap 'rm -rf "$hash_dir"' EXIT

assert_vector_inventory() {
  local pass="$1"
  echo "-- verify exact vector inventory ($pass)"
  grep -Ev '^[[:space:]]*(#|$)' "$vector_root/inventory.txt" | sort > "$hash_dir/vectors-expected.txt"
  for file in "$vector_root"/*.json; do
    basename "$file"
  done | sort > "$hash_dir/vectors-actual.txt"
  diff -u "$hash_dir/vectors-expected.txt" "$hash_dir/vectors-actual.txt"
}

echo "-- regenerate all vector families (first pass)"
go -C core test -count=1 ./internal/protocolcontract -update
go test -count=1 ./connectivity/v2signal -update
assert_vector_inventory "first pass"
sha256sum "$vector_root"/*.json | sort > "$hash_dir/vectors-first.sha256"

echo "-- regenerate all vector families (second pass)"
go -C core test -count=1 ./internal/protocolcontract -update
go test -count=1 ./connectivity/v2signal -update
assert_vector_inventory "second pass"
sha256sum "$vector_root"/*.json | sort > "$hash_dir/vectors-second.sha256"

echo "-- regenerations must be byte-identical"
diff "$hash_dir/vectors-first.sha256" "$hash_dir/vectors-second.sha256"

echo "-- committed vectors must match regeneration"
status="$(git -c core.quotepath=false status --short -- "$vector_root")"
if [ -n "$status" ]; then
  echo "regenerated vectors differ from committed $vector_root/:" >&2
  echo "$status" >&2
  exit 1
fi

echo "== vectors: PASS in ${SECONDS}s =="
