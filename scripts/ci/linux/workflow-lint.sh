#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== workflow-lint =="
actionlint -shellcheck= -pyflakes=
echo "== workflow-lint: PASS in ${SECONDS}s =="
