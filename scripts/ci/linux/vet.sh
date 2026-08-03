#!/usr/bin/env bash
# Linux preflight owns native compilation and vetting for both modules. It also
# owns the sole root build with go.work disabled, proving the released core
# dependency can be consumed without the workspace masking it.
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== vet =="

host_operating_system="$(go env GOHOSTOS)"
if [[ "$host_operating_system" != linux ]]; then
  echo "Linux compile/vet requires a Linux Go host, received: $host_operating_system" >&2
  exit 1
fi

echo "-- go build (root, native Linux)"
go build ./...

echo "-- go build (core, native Linux)"
go -C core build ./...

echo "-- go vet (root, native Linux)"
go vet ./...

echo "-- go vet (core, native Linux)"
go -C core vet ./...

echo "-- GOWORK=off root released-core consumer build"
GOWORK=off go build ./...

echo "== vet: PASS in ${SECONDS}s =="
