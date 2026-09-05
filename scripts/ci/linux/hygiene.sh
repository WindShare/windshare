#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== hygiene =="

existing_go_files() {
  while IFS= read -r -d '' file; do
    if [[ -f "$file" && "$file" != third_party/pion/ice/* && "$file" != third_party/pion/webrtc/* ]]; then
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

echo "-- Release source and binary packaging contracts"
go test ./scripts/ci/_sourcebundle ./scripts/ci/_releaseassets

echo "-- Pinned Pion source verifier tests"
go test ./scripts/ci/_piondeps

echo "-- Pinned Pion source and patch reproduction"
go run ./scripts/ci/_piondeps -reproduce

echo "-- Web production graph resolver tests"
node --test scripts/ci/web-forbidden.tests.mjs

echo "-- Browser FSA reviewed support artifact syntax"
while IFS= read -r -d '' script; do
  node --check "$script"
done < <(find web/scripts/browser-evidence-review/fsa-resumable-zip -type f -name '*.mjs' -print0)

echo "-- Browser FSA reviewed support artifacts"
node --test web/scripts/browser-evidence-review/fsa-resumable-zip/tests/*.test.mjs
node web/scripts/browser-evidence-review/fsa-resumable-zip/review.mjs >/dev/null

echo "-- Browser workspace ZIP review JavaScript syntax"
while IFS= read -r -d '' script; do
  node --check "$script"
done < <(find web/scripts/browser-evidence/workspace-zip-recommendation -type f -name '*.mjs' -print0)

echo "-- Browser workspace ZIP review contracts"
node --test web/scripts/browser-evidence/workspace-zip-recommendation/tests/*.test.mjs

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
