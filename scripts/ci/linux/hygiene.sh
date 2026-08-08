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

echo "-- Web retired paths and production graph"
node scripts/ci/web-forbidden.mjs

echo "-- Go retired paths and production graph"
node scripts/ci/go-v1-forbidden.mjs

echo "== hygiene: PASS in ${SECONDS}s =="
