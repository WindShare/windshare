#!/usr/bin/env bash
# The root short sweep still visits integration/E2E packages. A gate-owned run
# ID keeps any non-skipped loopback work correlated without changing its budget.
# JSON mode preserves successful scenario evidence that ordinary Go output hides.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority
source scripts/ci/test-run-id.sh

SECONDS=0
generated_run_id="$(new_windshare_test_run_id check)"
export WINDSHARE_TEST_RUN_ID="$generated_run_id"
echo "== check: run_id=$WINDSHARE_TEST_RUN_ID =="
windshare_go_test_json -short -count=1 ./...
windshare_go -C core test -short -count=1 ./...
windshare_go vet ./...
windshare_go -C core vet ./...
windshare_go_consumer pnpm -C web lint
windshare_go_consumer pnpm -C web exec tsc -b --force
windshare_go_consumer pnpm -C web run test:unit:remainder
echo "== check: PASS in ${SECONDS}s =="
