#!/usr/bin/env bash
# CI-parity sloc gate (Linux). Mirrors ci.yml job `sloc` verbatim: sloc-guard
# check. CI obtains sloc-guard from the doraemonkeys/sloc-guard action (latest
# release binary, cargo-install fallback); locally the binary is a PATH
# prerequisite — install with:
#   cargo install --git https://github.com/doraemonkeys/sloc-guard --locked
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== sloc =="

echo "-- sloc-guard check"
if ! command -v sloc-guard >/dev/null 2>&1; then
  echo "sloc-guard not found on PATH; install it first:" >&2
  echo "  cargo install --git https://github.com/doraemonkeys/sloc-guard --locked" >&2
  exit 1
fi
sloc-guard check

echo "== sloc: PASS in ${SECONDS}s =="
