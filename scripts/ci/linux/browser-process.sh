#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority
source scripts/ci/test-run-id.sh

SECONDS=0
generated_run_id="$(new_windshare_test_run_id browser-process)"
export WINDSHARE_TEST_RUN_ID="$generated_run_id"
echo "== browser-process: run_id=$WINDSHARE_TEST_RUN_ID =="
windshare_go_consumer pnpm -C web run test:browser:process:integration
windshare_go test -count=1 ./cmd/testprocessowner ./internal/testprocess ./internal/processowner/protocol ./internal/processowner/linuxsubreaper
echo "== browser-process: PASS in ${SECONDS}s =="
