#!/usr/bin/env bash
# The recorder writes canonical JSONL to test stdout. -json preserves successful
# output in a machine-readable Actions stream instead of exposing only failures.
set -euo pipefail
# Historical byte evidence remains exact, while this reviewed plan keeps
# maintenance-only edits from resetting the stability window.
readonly WINDSHARE_STABILITY_INTEGRATION_SEMANTICS='{"schema_version":"windshare.stability-helper-semantics/v1","operating_system":"linux","role":"integration-entrypoint","revision":3,"command_plan":["select-stability-evidence-mode","settle-go-authority","settle-run-identity","conditionally-publish-authenticated-product-start","invoke-retained-go-test-once"]}'

readonly stability_handshake_variable_count=3
stability_handshake_presence_count=0
if [[ -v WINDSHARE_STABILITY_START_REQUEST ]]; then
  ((stability_handshake_presence_count += 1))
fi
if [[ -v WINDSHARE_STABILITY_STARTED_OUTPUT ]]; then
  ((stability_handshake_presence_count += 1))
fi
if [[ -v WINDSHARE_STABILITY_START_SECRET ]]; then
  ((stability_handshake_presence_count += 1))
fi
if (( stability_handshake_presence_count != 0 &&
  stability_handshake_presence_count != stability_handshake_variable_count )); then
  echo "WindShare stability handshake is partial (present=$stability_handshake_presence_count required=$stability_handshake_variable_count)" >&2
  exit 1
fi
stability_evidence_mode=ordinary
if (( stability_handshake_presence_count == stability_handshake_variable_count )); then
  stability_evidence_mode=authenticated
fi
readonly stability_evidence_mode

cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority
source scripts/ci/test-run-id.sh

SECONDS=0
generated_run_id="$(new_windshare_test_run_id integration)"
export WINDSHARE_TEST_RUN_ID="$generated_run_id"
echo "== integration: run_id=$WINDSHARE_TEST_RUN_ID stability_evidence=$stability_evidence_mode =="
# Stability evidence begins only after retained Go and run identity have settled.
if [[ "$stability_evidence_mode" == authenticated ]]; then
  node scripts/ci/stability/result.mjs started
  unset WINDSHARE_STABILITY_START_REQUEST WINDSHARE_STABILITY_STARTED_OUTPUT WINDSHARE_STABILITY_START_SECRET
fi
windshare_go_test_json -count=1 ./integration/...
echo "== integration: PASS in ${SECONDS}s =="
