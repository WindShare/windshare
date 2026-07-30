#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

node scripts/ci/browsergate/main.mjs local --run-policy stability
exit $?
