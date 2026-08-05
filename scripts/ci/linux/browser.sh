#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
readonly CHROMIUM_SHORT_CONTRACT_PORT=4197
echo "== browser =="
pnpm -C web run test:browser:smoke
echo "-- Chromium short browser contracts"
WINDSHARE_CONTRACT_PORT="$CHROMIUM_SHORT_CONTRACT_PORT" pnpm -C web run test:browser:contract:short
echo "== browser: PASS in ${SECONDS}s =="
