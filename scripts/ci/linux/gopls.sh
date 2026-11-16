#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== gopls =="
node --test scripts/ci/gopls/check.integration.mjs
node scripts/ci/gopls/run.mjs
echo "== gopls: PASS in ${SECONDS}s =="
