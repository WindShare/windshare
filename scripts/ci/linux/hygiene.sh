#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== hygiene =="

existing_go_files() {
  while IFS= read -r -d '' file; do
    if [[ -f "$file" ]]; then
      printf '%s\0' "$file"
    fi
  done
}

echo "-- gofmt (tracked and untracked Go files)"
unformatted="$({ git ls-files -z --cached --others --exclude-standard -- '*.go'; } | existing_go_files | xargs -0 -r gofmt -l)"
if [[ -n "$unformatted" ]]; then
  echo "files need gofmt:" >&2
  printf '%s\n' "$unformatted" >&2
  exit 1
fi

echo "-- whitespace"
git --no-pager diff --check

echo "-- Frozen Unicode Go tables"
node scripts/unicode15/generate-go.mjs --check

echo "-- Web retired paths and production graph"
node scripts/ci/web-forbidden.mjs

echo "-- Go retired paths and production graph"
node scripts/ci/go-v1-forbidden.mjs

echo "-- Core production dependency boundary tests"
go test ./scripts/ci/_coreboundary

echo "-- Core production dependency boundary"
go run ./scripts/ci/_coreboundary

echo "-- Go validation package ownership tests"
go test ./scripts/ci/_gopackages

echo "-- Go validation package ownership"
go run ./scripts/ci/_gopackages -set all >/dev/null

echo "-- Named Go test suite selection tests"
go test ./scripts/ci/_gotestsuite

echo "== hygiene: PASS in ${SECONDS}s =="
