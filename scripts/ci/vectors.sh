#!/usr/bin/env bash
# CI-parity golden-vector idempotence gate (Linux). Mirrors ci.yml
# golden-vectors / work-plan §10.3 verbatim: regenerate every vector family
# twice; the regenerations must be byte-identical and must exactly match the
# committed testvectors/. Hashes stage in a mktemp dir (CI uses RUNNER_TEMP).
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== vectors =="

hash_dir="$(mktemp -d)"
trap 'rm -rf "$hash_dir"' EXIT

echo "-- regenerate all vector families (first pass)"
go -C core test -count=1 ./share -update
go test -count=1 ./relay/protocol -update
sha256sum testvectors/*.json | sort > "$hash_dir/vectors-first.sha256"

echo "-- regenerate all vector families (second pass)"
go -C core test -count=1 ./share -update
go test -count=1 ./relay/protocol -update
sha256sum testvectors/*.json | sort > "$hash_dir/vectors-second.sha256"

echo "-- regenerations must be byte-identical"
diff "$hash_dir/vectors-first.sha256" "$hash_dir/vectors-second.sha256"

echo "-- committed vectors must match regeneration"
status="$(git -c core.quotepath=false status --short -- testvectors)"
if [ -n "$status" ]; then
  echo "regenerated vectors differ from committed testvectors/:" >&2
  echo "$status" >&2
  exit 1
fi

echo "== vectors: PASS in ${SECONDS}s =="
