#!/usr/bin/env bash
# Browsergate owns orchestration and final reduction; this file is only the
# native Make boundary and deliberately forwards the supported local options.
set -euo pipefail
cd "$(dirname "$0")/../../.."

exec node scripts/ci/browsergate/main.mjs local "$@"
