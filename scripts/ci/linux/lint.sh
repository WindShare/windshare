#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/linux/go-package-sets.sh
windshare_load_go_package_set all

SECONDS=0
echo "== lint =="

echo "-- golangci-lint (production packages)"
# golangci-lint treats import paths as filesystem paths, so use the same
# complete pattern after the package authority has validated its expansion.
golangci-lint run ./...

echo "== lint: PASS in ${SECONDS}s =="
