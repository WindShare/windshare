#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

exec node scripts/ci/browsergate/main.mjs local --run-policy stability
