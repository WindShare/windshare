#!/usr/bin/env bash
# The Node orchestrator is the single semantic authority for sample isolation,
# suite continuation, guarding, and final reduction. This wrapper remains the
# thin Makefile/host boundary and deliberately forwards a safe --plan mode.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

node scripts/ci/browsergate/main.mjs local "$@"
exit $?
