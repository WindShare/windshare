#!/usr/bin/env bash
# One Browsergate command owns PR contract and generated-semantic validation so
# hosted and focused runs cannot accidentally acquire separate evidence owners.
set -euo pipefail
cd "$(dirname "$0")/../../.."

exec node scripts/ci/browsergate/main.mjs preflight
