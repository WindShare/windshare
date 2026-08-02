#!/usr/bin/env bash
# The lockfile is the sole dependency authority shared by independently
# invokable web and browser gates.
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== web-dependencies =="
pnpm -C web install --frozen-lockfile
echo "== web-dependencies: PASS in ${SECONDS}s =="
