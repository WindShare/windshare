#!/usr/bin/env bash
# The external network matrix is intentionally outside the blocking PR gate.
# With no explicit authority configuration this entry emits canonical
# unavailable/not-executed evidence without acquiring helpers or credentials.
set -euo pipefail
cd "$(dirname "$0")/../.."

node scripts/ci/browsergate/network-entry.mjs "$@"
