#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== gopls =="

# Verify the complete dependency projection before any file reaches gopls.
# A failed producer in a pipeline must not launch analysis on a partial set.
sources="$(mktemp)"
trap 'rm -f "$sources"' EXIT
go run ./scripts/ci/_piondeps -maintained-go-files -0 >"$sources"
diagnostics="$(xargs -0 -r gopls check -severity=hint <"$sources")"
if [[ -n "$diagnostics" ]]; then
  printf '%s\n' "$diagnostics"
  echo "gopls reported diagnostics" >&2
  exit 1
fi

echo "== gopls: PASS in ${SECONDS}s =="
