#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== gopls =="

existing_go_files() {
  while IFS= read -r -d '' file; do
    if [[ -f "$file" ]]; then
      printf '%s\0' "$file"
    fi
  done
}

diagnostics="$(git ls-files -z -- '*.go' | existing_go_files | xargs -0 -r gopls check -severity=hint)"
if [[ -n "$diagnostics" ]]; then
  printf '%s\n' "$diagnostics"
  echo "gopls reported diagnostics" >&2
  exit 1
fi

echo "== gopls: PASS in ${SECONDS}s =="
