#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/linux/go-package-sets.sh
windshare_load_go_package_set all
all_packages=("${WINDSHARE_GO_PACKAGES[@]}")

SECONDS=0
echo "== race =="

echo "-- production short race sweep"
go test -short -race -count=1 "${all_packages[@]}"

echo "== race: PASS in ${SECONDS}s =="
