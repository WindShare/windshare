#!/usr/bin/env bash
# CI-parity hygiene gate (Linux). Mirrors ci.yml job `hygiene`
# (sloc-guard is standalone): formatting, thin dispatch, native run identity,
# stability and exact-SHA release-readiness contracts, argument batching,
# source-only product/security scans, and gopls diagnostics.
# Deviation from CI: gopls diagnostics are captured in a mktemp file instead
# of ./gopls-diagnostics.txt so the gate never dirties the worktree.
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

echo "-- thin CI dispatch contract"
node scripts/ci/contract.tests.mjs
node scripts/ci/contract.mjs

echo "-- native run-ID contract"
bash scripts/ci/test-run-id-entrypoints.tests.sh

echo "-- stability evidence contracts"
node scripts/ci/stability/result.tests.mjs
node scripts/ci/stability/release-reducer.tests.mjs
node --test scripts/ci/stability/workflow.tests.mjs

echo "-- exact-SHA release-readiness contracts"
node --test scripts/ci/release-readiness/resolver.tests.mjs
node --test scripts/ci/release-readiness/verifier.tests.mjs
node --test scripts/ci/release-readiness/workflow.tests.mjs

echo "-- Windows native argument batching contract"
pwsh -NoProfile -File scripts/ci/hygiene/native-argument-batches.tests.ps1

echo "-- Web v1 forbidden references (source-only)"
node scripts/ci/web-forbidden.mjs --source-only

echo "-- Go v1 forbidden roots and production dependencies"
node scripts/ci/go-v1-forbidden.mjs

echo "-- gopls check (severity=hint, tracked Go files)"
gopls_version="$(tr -d '\r\n' < scripts/ci/gopls.version)"
if [[ ! "$gopls_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "gopls.version is not a canonical release version: $gopls_version" >&2
  exit 1
fi
go install "golang.org/x/tools/gopls@$gopls_version"
gopls_bin="$(go env GOPATH)/bin/gopls"
diagnostics_file="$(mktemp)"
trap 'rm -f "$diagnostics_file"' EXIT
git ls-files -z -- '*.go' | existing_go_files | xargs -0 -r "$gopls_bin" check -severity=hint | tee "$diagnostics_file"
if [ -s "$diagnostics_file" ]; then
  echo "gopls reported diagnostics" >&2
  exit 1
fi

echo "== hygiene: PASS in ${SECONDS}s =="
