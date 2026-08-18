#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== e2e =="
go run ./scripts/ci/_gotestsuite -run '^TestUserTraceCriticalSenderRelayReceiver$' ./e2e
echo "== e2e: PASS in ${SECONDS}s =="
