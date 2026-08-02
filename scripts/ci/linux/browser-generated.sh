#!/usr/bin/env bash
# Generated semantic evidence runs in a real child process so module-cache state
# from the contract suite cannot make checked-in artifacts appear current.
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== browser-generated =="
pnpm -C web run test:browser:generated-semantic:process
echo "== browser-generated: PASS in ${SECONDS}s =="
