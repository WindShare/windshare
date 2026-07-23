#!/usr/bin/env bash
# CI-parity hygiene gate (Linux). Mirrors ci.yml job `hygiene` verbatim
# (sloc-guard lives in the standalone `sloc` gate since 2026-07-14): gofmt
# over tracked Go files, `git diff --check` against the empty tree, a source-only
# Go/Web v1 forbidden scans, short R8 evidence contracts, and gopls check
# -severity=hint.
# Deviation from CI: gopls diagnostics are captured in a mktemp file instead
# of ./gopls-diagnostics.txt so the gate never dirties the worktree.
set -euo pipefail
cd "$(dirname "$0")/../.."

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

echo "-- Web v1 forbidden references (source-only)"
node scripts/ci/web-forbidden.mjs --source-only

echo "-- Go v1 forbidden roots and production dependencies"
node scripts/ci/go-v1-forbidden.mjs

echo "-- R8 performance evidence contracts"
for suite in \
  scripts/go-benchmark-evidence.tests.ps1 \
  scripts/r8-go-evidence.tests.ps1 \
  scripts/r8-evidence-summary.tests.ps1 \
  scripts/r8-web-performance-authority.tests.ps1; do
  pwsh -NoProfile -File "$suite"
done

echo "-- gopls check (severity=hint, tracked Go files)"
go install golang.org/x/tools/gopls@v0.22.0
gopls_bin="$(go env GOPATH)/bin/gopls"
diagnostics_file="$(mktemp)"
trap 'rm -f "$diagnostics_file"' EXIT
git ls-files -z -- '*.go' | existing_go_files | xargs -0 -r "$gopls_bin" check -severity=hint | tee "$diagnostics_file"
if [ -s "$diagnostics_file" ]; then
  echo "gopls reported diagnostics" >&2
  exit 1
fi

echo "== hygiene: PASS in ${SECONDS}s =="
