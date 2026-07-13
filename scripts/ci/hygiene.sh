#!/usr/bin/env bash
# CI-parity hygiene gate (Linux). Mirrors ci.yml jobs `hygiene` + `sloc`
# verbatim: gofmt over tracked Go files, `git diff --check` against the empty
# tree, gopls check -severity=hint (same pinned gopls@v0.22.0 install), then
# sloc-guard check. CI obtains sloc-guard from the doraemonkeys/sloc-guard
# action (latest release binary, cargo-install fallback); locally the binary
# is a PATH prerequisite — install with:
#   cargo install --git https://github.com/doraemonkeys/sloc-guard --locked
# Deviation from CI: gopls diagnostics are captured in a mktemp file instead
# of ./gopls-diagnostics.txt so the gate never dirties the worktree.
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== hygiene =="

echo "-- gofmt (tracked Go files)"
unformatted="$(git ls-files -z -- '*.go' | xargs -0 gofmt -l)"
if [ -n "$unformatted" ]; then
  echo "files need gofmt:" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "-- whitespace (git diff --check against the empty tree)"
# --no-pager: in an interactive terminal git would otherwise hand the diff to
# less and park the whole gate on a keypress; gate scripts must never page.
git --no-pager diff --check "$(git hash-object -t tree /dev/null)"

echo "-- gopls check (severity=hint, tracked Go files)"
go install golang.org/x/tools/gopls@v0.22.0
gopls_bin="$(go env GOPATH)/bin/gopls"
diagnostics_file="$(mktemp)"
trap 'rm -f "$diagnostics_file"' EXIT
git ls-files -z -- '*.go' | xargs -0 "$gopls_bin" check -severity=hint | tee "$diagnostics_file"
if [ -s "$diagnostics_file" ]; then
  echo "gopls reported diagnostics" >&2
  exit 1
fi

echo "-- sloc-guard check"
if ! command -v sloc-guard >/dev/null 2>&1; then
  echo "sloc-guard not found on PATH; install it first:" >&2
  echo "  cargo install --git https://github.com/doraemonkeys/sloc-guard --locked" >&2
  exit 1
fi
sloc-guard check

echo "== hygiene: PASS in ${SECONDS}s =="
