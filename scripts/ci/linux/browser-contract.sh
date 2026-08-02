#!/usr/bin/env bash
# Fast browsergate semantic contract entrypoint. Product browser execution and
# real process fixtures remain in their dedicated gates.
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== browser-contract =="
pnpm -C web run test:browser:evidence:contract
echo "== browser-contract: PASS in ${SECONDS}s =="
