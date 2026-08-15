#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/linux/go-package-sets.sh
windshare_load_go_package_set all
all_packages=("${WINDSHARE_GO_PACKAGES[@]}")

SECONDS=0
echo "== vet =="

echo "-- go vet (production packages)"
go vet "${all_packages[@]}"

echo "== vet: PASS in ${SECONDS}s =="
