#!/usr/bin/env bash
set -euo pipefail

readonly artifact_only_version=v0.0.0-ci
readonly closed_release_version=v0.3.0

fail() {
  echo "core release ref: $1" >&2
  exit 1
}

validate_sha() {
  if [[ ! "$1" =~ ^[0-9a-f]{40}$ ]]; then
    fail "candidate must be an exact lowercase 40-character commit SHA"
  fi
}

validate_version() {
  if [[ ! "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]]; then
    fail "candidate version is not a safe semantic version: $1"
  fi
  if [ "$1" = "$artifact_only_version" ]; then
    fail "release version is reserved for non-publishing artifact checks: $1"
  fi
  if [ "$1" = "$closed_release_version" ]; then
    fail "release version is closed and cannot be verified again: $1"
  fi
}

require_direct_commit_ref() {
  local ref="$1"
  local expected_sha="$2"
  local label="$3"
  local object_sha
  local object_type

  if ! object_sha="$(git rev-parse --verify "$ref" 2>/dev/null)"; then
    fail "$label does not exist: $ref"
  fi
  if ! object_type="$(git cat-file -t "$object_sha" 2>/dev/null)"; then
    fail "$label object cannot be inspected: $ref"
  fi
  if [ "$object_type" != "commit" ]; then
    fail "$label must directly reference a commit; annotated tags are not release evidence"
  fi
  if [ "$object_sha" != "$expected_sha" ]; then
    fail "$label does not equal the expected commit"
  fi
}

event_name="${EVENT_NAME:-}"
if [ "$event_name" = "push" ]; then
  event_created="${EVENT_CREATED:-}"
  event_deleted="${EVENT_DELETED:-}"
  event_forced="${EVENT_FORCED:-}"
  event_before="${EVENT_BEFORE:-}"
  event_after="${EVENT_AFTER:-}"
  event_ref="${EVENT_REF:-}"
  event_sha="${EVENT_SHA:-}"
  zero_sha=0000000000000000000000000000000000000000

  validate_sha "$event_sha"
  if [ "$event_created" != "true" ] || [ "$event_deleted" != "false" ] ||
    [ "$event_forced" != "false" ] || [ "$event_before" != "$zero_sha" ]; then
    fail "release verification accepts only a newly created, non-forced tag"
  fi
  if [ "$event_after" != "$event_sha" ]; then
    fail "tag event after SHA does not match github.sha"
  fi

  checked_out_sha="$(git rev-parse HEAD)"
  if [ "$checked_out_sha" != "$event_sha" ]; then
    fail "candidate resolver checkout does not match github.sha"
  fi
  require_direct_commit_ref "$event_ref" "$event_sha" "event tag"

  if [[ "$event_ref" == refs/tags/core-candidate/* ]]; then
    candidate_path="${event_ref#refs/tags/core-candidate/}"
    version="${candidate_path%/*}"
    commit_sha="${candidate_path##*/}"
    validate_version "$version"
    validate_sha "$commit_sha"
    expected_ref="refs/tags/core-candidate/$version/$commit_sha"
    if [ "$event_ref" != "$expected_ref" ]; then
      fail "candidate tag must be core-candidate/<version>/<40-char-sha>"
    fi
    if [ "$commit_sha" != "$event_sha" ]; then
      fail "candidate tag SHA suffix does not match github.sha"
    fi
  elif [[ "$event_ref" == refs/tags/core/v* ]]; then
    commit_sha="$event_sha"
    version="${event_ref#refs/tags/core/}"
    validate_version "$version"
    candidate_ref="refs/tags/core-candidate/$version/$commit_sha"
    require_direct_commit_ref "$candidate_ref" "$commit_sha" "matching candidate tag"
  else
    fail "unexpected release ref: $event_ref"
  fi
elif [ "$event_name" = "workflow_dispatch" ]; then
  commit_sha="${INPUT_COMMIT_SHA:-}"
  version="${INPUT_VERSION:-}"
  validate_sha "$commit_sha"
  validate_version "$version"
  if [ "$(git cat-file -t "$commit_sha" 2>/dev/null || true)" != "commit" ]; then
    fail "manual candidate is not an available commit object"
  fi
else
  fail "unsupported release event: $event_name"
fi

if [ -z "${GITHUB_OUTPUT:-}" ]; then
  fail "GITHUB_OUTPUT is required"
fi
printf 'commit_sha=%s\n' "$commit_sha" >>"$GITHUB_OUTPUT"
printf 'version=%s\n' "$version" >>"$GITHUB_OUTPUT"
