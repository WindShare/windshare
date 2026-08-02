#!/usr/bin/env bash
# CI-parity hygiene gate (Linux). Mirrors ci.yml job `hygiene` verbatim
# (sloc-guard lives in the standalone `sloc` gate since 2026-07-14): gofmt
# over tracked Go files, `git diff --check` against the empty tree, clone-visible
# Makefile gate scripts, explicit workflow shell invocation, live targets for
# static shell content assertions, Windows native argument batching, source-only
# Go/Web v1 forbidden scans, shared evidence helper contracts, and gopls check
# -severity=hint.
# Deviation from CI: gopls diagnostics are captured in a mktemp file instead
# of ./gopls-diagnostics.txt so the gate never dirties the worktree.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority

SECONDS=0
echo "== hygiene =="

existing_go_files() {
  while IFS= read -r -d '' file; do
    if [[ -f "$file" ]]; then
      printf '%s\0' "$file"
    fi
  done
}

echo "-- gofmt (tracked Go files)"
unformatted="$(git ls-files -z -- '*.go' | existing_go_files | xargs -0 -r gofmt -l)"
if [ -n "$unformatted" ]; then
  echo "files need gofmt:" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "-- whitespace (git diff --check against the empty tree)"
# --no-pager: in an interactive terminal git would otherwise hand the diff to
# less and park the whole gate on a keypress; gate scripts must never page.
git --no-pager diff --check "$(git hash-object -t tree /dev/null)"

echo "-- CI checkout contract"
node scripts/ci/contract.tests.mjs
node scripts/ci/contract.mjs
node scripts/ci/makeauthority/entry.tests.mjs
bash scripts/ci/makeauthority/authority.tests.sh
node scripts/ci/goauthority/inventory.tests.mjs
node scripts/ci/goauthority/test-json-entrypoints.tests.mjs
bash scripts/ci/goauthority/authority.tests.sh

echo "-- stability evidence contracts"
node scripts/ci/stability/result.tests.mjs
node scripts/ci/stability/release-reducer.tests.mjs
pwsh -NoProfile -File scripts/ci/test-run-id-entrypoints.tests.ps1

echo "-- Windows native argument batching contract"
pwsh -NoProfile -File scripts/ci/hygiene/native-argument-batches.tests.ps1

echo "-- Web v1 forbidden references (source-only)"
node scripts/ci/web-forbidden.mjs --source-only

echo "-- Go v1 forbidden roots and production dependencies"
windshare_go_consumer node scripts/ci/go-v1-forbidden.mjs

echo "-- gopls check (severity=hint, tracked Go files)"
gopls_version="$(tr -d '\r\n' < scripts/ci/gopls.version)"
if [[ ! "$gopls_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "gopls.version is not a canonical release version: $gopls_version" >&2
  exit 1
fi
windshare_go install "golang.org/x/tools/gopls@$gopls_version"
gopls_bin="$(windshare_go env GOPATH)/bin/gopls"
diagnostics_file="$(mktemp)"
trap 'rm -f "$diagnostics_file"' EXIT
git ls-files -z -- '*.go' | existing_go_files | xargs -0 -r "$gopls_bin" check -severity=hint | tee "$diagnostics_file"
if [ -s "$diagnostics_file" ]; then
  echo "gopls reported diagnostics" >&2
  exit 1
fi

echo "== hygiene: PASS in ${SECONDS}s =="
