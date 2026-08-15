#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/linux/go-package-sets.sh
windshare_load_go_package_set all
all_packages=("${WINDSHARE_GO_PACKAGES[@]}")

SECONDS=0
echo "== check =="

echo "-- production short tests"
go test -short "${all_packages[@]}"

echo "-- Web typecheck"
pnpm -C web exec tsc -b --force

echo "-- Web unit tests"
pnpm -C web test

echo "== check: PASS in ${SECONDS}s =="
