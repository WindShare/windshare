#!/usr/bin/env bash

readonly WINDSHARE_STABILITY_RUN_ID_SEMANTICS='{"schema_version":"windshare.stability-helper-semantics/v1","operating_system":"linux","role":"test-run-identity-authority","revision":1,"command_plan":["require-settled-go-authority","bind-caller-seed","append-csprng-entropy"]}'
new_windshare_test_run_id() {
  local suite="$1"
  local seed
  local entropy
  local run_id
  local maximum_portable_token_bytes=128
  local portable_token_pattern='^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$'
  windshare_assert_go_authority_active || return 1
  if [[ -v WINDSHARE_TEST_RUN_ID ]]; then
    seed="$WINDSHARE_TEST_RUN_ID"
  else
    seed='local'
  fi
  if [[ ! "$suite" =~ ^[a-z0-9-]+$ ]]; then
    echo "invalid WindShare test suite identity: $suite" >&2
    return 1
  fi
  if [[ ! "$seed" =~ $portable_token_pattern ]]; then
    echo "WINDSHARE_TEST_RUN_ID must be an ASCII portable token without edge punctuation" >&2
    return 1
  fi

  # /dev/urandom supplies an explicit 128-bit collision boundary without
  # weakening the validated caller identity through lossy sanitization.
  entropy="$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"
  if [[ ! "$entropy" =~ ^[a-f0-9]{32}$ ]]; then
    echo "could not obtain 128-bit WindShare test-run entropy" >&2
    return 1
  fi
  run_id="${seed}-${suite}-${entropy}"
  if (( ${#run_id} > maximum_portable_token_bytes )) ||
    [[ ! "$run_id" =~ $portable_token_pattern ]]; then
    echo "WINDSHARE_TEST_RUN_ID seed is too long for the ${maximum_portable_token_bytes}-byte portable token contract" >&2
    return 1
  fi
  printf '%s\n' "$run_id"
}
