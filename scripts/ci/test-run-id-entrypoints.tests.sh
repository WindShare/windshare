#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$repository_root"
helper_path="$repository_root/scripts/ci/test-run-id.sh"
caller_run_id='entrypoint-contract-seed'
had_run_id=0
previous_run_id=''
if [[ -v WINDSHARE_TEST_RUN_ID ]]; then
  had_run_id=1
  previous_run_id="$WINDSHARE_TEST_RUN_ID"
fi

restore_run_id() {
  if (( had_run_id == 1 )); then
    export WINDSHARE_TEST_RUN_ID="$previous_run_id"
  else
    unset WINDSHARE_TEST_RUN_ID
  fi
}
trap restore_run_id EXIT

fail() {
  echo "test run-ID Bash entrypoint contracts: $1" >&2
  exit 1
}

count_occurrences() {
  local remaining="$1"
  local needle="$2"
  local count=0

  while [[ "$remaining" == *"$needle"* ]]; do
    remaining="${remaining#*"$needle"}"
    ((count += 1))
  done
  printf '%s\n' "$count"
}

line_number() {
  local path="$1"
  local needle="$2"
  local match

  match="$(grep -nF -- "$needle" "$path")" || fail "missing expected command in $path: $needle"
  printf '%s\n' "${match%%:*}"
}

helper_source="$(<"$helper_path")"
retired_helper_tokens=(
  'WINDSHARE_STABILITY_RUN_ID_SEMANTICS'
  'stability-helper-semantics'
  'windshare_assert_go_authority_active'
)
for retired_token in "${retired_helper_tokens[@]}"; do
  [[ "$helper_source" != *"$retired_token"* ]] ||
    fail "test-run-id.sh retains retired control-plane coupling: $retired_token"
done

# shellcheck source=scripts/ci/test-run-id.sh
source "$helper_path"
export WINDSHARE_TEST_RUN_ID="$caller_run_id"

direct_run_id="$(new_windshare_test_run_id check)"
[[ "$direct_run_id" =~ ^${caller_run_id}-check-[a-f0-9]{32}$ ]] ||
  fail 'direct run-ID construction did not preserve the validated seed and 128-bit suffix'
[[ "$WINDSHARE_TEST_RUN_ID" == "$caller_run_id" ]] ||
  fail 'pure run-ID construction mutated the caller environment'

export WINDSHARE_TEST_RUN_ID=x
single_character_seed_run_id="$(new_windshare_test_run_id check)"
[[ "$single_character_seed_run_id" =~ ^x-check-[a-f0-9]{32}$ ]] ||
  fail 'a one-character portable seed was rejected or rewritten'
export WINDSHARE_TEST_RUN_ID=.invalid
if new_windshare_test_run_id check >/dev/null 2>&1; then
  fail 'edge-punctuation seed did not fail'
fi
export WINDSHARE_TEST_RUN_ID="$caller_run_id"
if new_windshare_test_run_id 'Invalid-Suite' >/dev/null 2>&1; then
  fail 'non-canonical suite did not fail'
fi

entrypoints=(
  'check|scripts/ci/linux/check.sh'
  'race|scripts/ci/linux/race.sh'
  'coverage|scripts/ci/linux/coverage.sh'
  'integration|scripts/ci/linux/integration.sh'
  'e2e-go|scripts/ci/linux/e2e-go.sh'
  'browser-process|scripts/ci/linux/browser-process.sh'
)

for entrypoint in "${entrypoints[@]}"; do
  IFS='|' read -r suite path <<<"$entrypoint"
  source_text="$(<"$path")"
  invocation="new_windshare_test_run_id $suite"

  [[ "$(count_occurrences "$source_text" "$invocation")" == 1 ]] ||
    fail "$path must construct exactly one run ID for $suite"
  [[ "$(count_occurrences "$source_text" 'set -euo pipefail')" == 1 ]] ||
    fail "$path must propagate child failures through its Bash process boundary"
  retired_entrypoint_tokens=(
    'goauthority/authority.sh'
    'windshare_enter_go_authority'
    'windshare_go'
  )
  for retired_token in "${retired_entrypoint_tokens[@]}"; do
    [[ "$source_text" != *"$retired_token"* ]] ||
      fail "$path retains retired Go control-plane coupling: $retired_token"
  done

  run_id="$(new_windshare_test_run_id "$suite")"
  [[ "$run_id" =~ ^${caller_run_id}-${suite}-[a-f0-9]{32}$ ]] ||
    fail "$suite did not preserve one invocation-owned run identity"
  [[ "$WINDSHARE_TEST_RUN_ID" == "$caller_run_id" ]] ||
    fail "$suite construction mutated the caller run ID"
done

integration_path='scripts/ci/linux/integration.sh'
integration_source="$(<"$integration_path")"
integration_test='go test -json -count=1 ./integration/...'
[[ "$(count_occurrences "$integration_source" 'go test -json')" == 1 ]] ||
  fail 'Linux integration must contain exactly one local go test -json execution'
[[ "$(count_occurrences "$integration_source" "$integration_test")" == 1 ]] ||
  fail 'Linux integration local go test -json command diverged from its contract'
[[ "$(count_occurrences "$integration_source" 'node scripts/ci/stability/result.mjs started')" == 1 ]] ||
  fail 'Linux integration must publish exactly one authenticated started event'

authenticated_line="$(line_number "$integration_path" 'if [[ "$stability_evidence_mode" == authenticated ]]; then')"
identity_line="$(line_number "$integration_path" 'export WINDSHARE_TEST_RUN_ID="$generated_run_id"')"
started_line="$(line_number "$integration_path" 'node scripts/ci/stability/result.mjs started')"
secret_cleanup_line="$(line_number "$integration_path" 'unset WINDSHARE_STABILITY_START_REQUEST')"
test_line="$(line_number "$integration_path" "$integration_test")"
if (( authenticated_line >= started_line ||
      identity_line >= started_line ||
      started_line >= secret_cleanup_line ||
      secret_cleanup_line >= test_line )); then
  fail 'Linux integration must settle run identity before publishing the authenticated start event'
fi

restore_run_id
trap - EXIT
echo 'test run-ID Bash entrypoint contracts: PASS'
