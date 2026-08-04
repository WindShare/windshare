#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== browser =="
pnpm -C web run test:browser:smoke
echo "== browser: PASS in ${SECONDS}s =="
