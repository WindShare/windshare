#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== root-release-graph =="

# The workspace intentionally exposes current core source. Disabling it here
# proves that the root module remains buildable from its published dependency.
echo "-- root build against released core"
GOWORK=off go build ./...

echo "== root-release-graph: PASS in ${SECONDS}s =="
