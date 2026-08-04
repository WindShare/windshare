#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== sloc =="
sloc-guard check
echo "== sloc: PASS in ${SECONDS}s =="
