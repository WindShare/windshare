#!/usr/bin/env bash
set -euo pipefail

readonly candidate_ref_prefix=refs/tags/core-candidate/
readonly release_ref_prefix=refs/tags/core/
readonly zero_sha=0000000000000000000000000000000000000000

fail() {
  echo "core release ref: $1" >&2
  exit 1
}

validate_sha() {
  if [[ ! "${1:-}" =~ ^[0-9a-f]{40}$ ]]; then
    fail "candidate must be an exact lowercase 40-character commit SHA"
  fi
}

validate_version() {
  if [[ ! "${1:-}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    fail "candidate version must have the form vX.Y.Z without leading zeroes: ${1:-}"
  fi
}

require_commit_object() {
  local object_sha="$1"
  local label="$2"
  local object_type

  if ! object_type="$(git cat-file -t "$object_sha" 2>/dev/null)"; then
    fail "$label is not an available Git object"
  fi
  if [ "$object_type" != "commit" ]; then
    fail "$label must identify a commit object"
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
    fail "$label must directly reference a commit"
  fi
  if [ "$object_sha" != "$expected_sha" ]; then
    fail "$label does not equal the expected commit"
  fi
}

resolve_push_candidate() {
  local event_created="${EVENT_CREATED:-}"
  local event_deleted="${EVENT_DELETED:-}"
  local event_forced="${EVENT_FORCED:-}"
  local event_before="${EVENT_BEFORE:-}"
  local event_after="${EVENT_AFTER:-}"
  local event_ref="${EVENT_REF:-}"
  local event_sha="${EVENT_SHA:-}"
  local candidate_name
  local candidate_path
  local checked_out_sha

  if [[ "$event_ref" != "$candidate_ref_prefix"* ]]; then
    fail "unexpected candidate ref: $event_ref"
  fi
  candidate_path="${event_ref#"$candidate_ref_prefix"}"
  resolved_version="${candidate_path%%/*}"
  candidate_name="${candidate_path#*/}"
  validate_version "$resolved_version"
  if [ "$candidate_path" = "$resolved_version" ] || [ -z "$candidate_name" ]; then
    fail "candidate tag must have a non-empty name after core-candidate/<version>/"
  fi

  validate_sha "$event_sha"
  resolved_commit_sha="$event_sha"
  if [ "$event_created" != "true" ] || [ "$event_deleted" != "false" ] ||
    [ "$event_forced" != "false" ] || [ "$event_before" != "$zero_sha" ]; then
    fail "candidate resolution accepts only a newly created, non-forced tag"
  fi
  if [ "$event_after" != "$event_sha" ]; then
    fail "tag event after SHA does not match github.sha"
  fi
  require_commit_object "$event_sha" "candidate SHA"
  if ! checked_out_sha="$(git rev-parse --verify HEAD 2>/dev/null)"; then
    fail "candidate resolver checkout cannot be inspected"
  fi
  if [ "$checked_out_sha" != "$event_sha" ]; then
    fail "candidate resolver checkout does not match github.sha"
  fi
  require_direct_commit_ref "$event_ref" "$event_sha" "candidate tag"
}

resolve_manual_candidate() {
  resolved_commit_sha="${INPUT_COMMIT_SHA:-}"
  resolved_version="${INPUT_VERSION:-}"
  validate_sha "$resolved_commit_sha"
  validate_version "$resolved_version"
  require_commit_object "$resolved_commit_sha" "manual candidate"
}

write_resolved_candidate() {
  if [ -z "${GITHUB_OUTPUT:-}" ]; then
    fail "GITHUB_OUTPUT is required"
  fi
  printf 'commit_sha=%s\n' "$resolved_commit_sha" >>"$GITHUB_OUTPUT"
  printf 'version=%s\n' "$resolved_version" >>"$GITHUB_OUTPUT"
}

resolve_candidate() {
  resolved_commit_sha=""
  resolved_version=""

  case "${EVENT_NAME:-}" in
    push)
      resolve_push_candidate
      ;;
    workflow_dispatch)
      resolve_manual_candidate
      ;;
    *)
      fail "unsupported release event: ${EVENT_NAME:-}"
      ;;
  esac
  printf 'core release resolve: event=%s version=%s commit=%s\n' \
    "${EVENT_NAME:-}" "$resolved_version" "$resolved_commit_sha"
  write_resolved_candidate
}

read_remote_release_ref() {
  local remote_name="$1"
  local release_ref="$2"
  local advertised_refs
  local object_ref
  local object_sha
  local direct_object=""
  local peeled_commit=""

  remote_ref_exists=false
  remote_ref_commit=""
  if ! advertised_refs="$(git ls-remote "$remote_name" "$release_ref" "${release_ref}^{}")"; then
    fail "cannot inspect $release_ref on remote $remote_name"
  fi
  while read -r object_sha object_ref; do
    if [ -z "${object_sha:-}" ]; then
      continue
    fi
    if [[ ! "$object_sha" =~ ^[0-9a-f]{40}$ ]]; then
      fail "remote $remote_name advertised an invalid object for $release_ref"
    fi
    case "$object_ref" in
      "$release_ref")
        [ -z "$direct_object" ] || fail "remote $remote_name advertised $release_ref more than once"
        direct_object="$object_sha"
        ;;
      "${release_ref}^{}")
        [ -z "$peeled_commit" ] || fail "remote $remote_name advertised a duplicate peeled $release_ref"
        peeled_commit="$object_sha"
        ;;
      *)
        fail "remote $remote_name returned an unexpected ref while inspecting $release_ref"
        ;;
    esac
  done <<<"$advertised_refs"

  if [ -z "$direct_object" ]; then
    [ -z "$peeled_commit" ] || fail "remote $remote_name advertised a peeled ref without $release_ref"
    return
  fi
  remote_ref_exists=true
  if [ -n "$peeled_commit" ]; then
    remote_ref_commit="$peeled_commit"
  else
    remote_ref_commit="$direct_object"
  fi
}

log_publish_state() {
  local state="$1"
  local version="$2"
  local commit_sha="$3"
  local release_ref="$4"
  printf 'core release publish: state=%s version=%s commit=%s ref=%s\n' \
    "$state" "$version" "$commit_sha" "$release_ref"
}

publish_candidate() {
  local version="${CORE_RELEASE_VERSION:-}"
  local commit_sha="${CORE_RELEASE_COMMIT_SHA:-}"
  local remote_name="${CORE_RELEASE_REMOTE:-origin}"
  local release_ref
  local checked_out_sha

  validate_version "$version"
  validate_sha "$commit_sha"
  require_commit_object "$commit_sha" "publish candidate"
  if ! checked_out_sha="$(git rev-parse --verify HEAD 2>/dev/null)"; then
    fail "publish checkout cannot be inspected"
  fi
  if [ "$checked_out_sha" != "$commit_sha" ]; then
    fail "publish checkout does not match the candidate commit"
  fi
  if ! git remote get-url "$remote_name" >/dev/null 2>&1; then
    fail "publish remote does not exist: $remote_name"
  fi

  release_ref="$release_ref_prefix$version"
  read_remote_release_ref "$remote_name" "$release_ref"
  if [ "$remote_ref_exists" = true ]; then
    if [ "$remote_ref_commit" = "$commit_sha" ]; then
      log_publish_state already-current "$version" "$commit_sha" "$release_ref"
      return
    fi
    fail "$release_ref already points to $remote_ref_commit; refusing to move it to $commit_sha"
  fi

  log_publish_state creating "$version" "$commit_sha" "$release_ref"
  if git push "$remote_name" "$commit_sha:$release_ref"; then
    read_remote_release_ref "$remote_name" "$release_ref"
    if [ "$remote_ref_exists" = true ] && [ "$remote_ref_commit" = "$commit_sha" ]; then
      log_publish_state created "$version" "$commit_sha" "$release_ref"
      return
    fi
    fail "$release_ref was not created at the candidate commit"
  fi

  # A competing idempotent publisher may win between the read and the create.
  # Re-reading turns that benign race into success without ever forcing a tag.
  read_remote_release_ref "$remote_name" "$release_ref"
  if [ "$remote_ref_exists" = true ] && [ "$remote_ref_commit" = "$commit_sha" ]; then
    log_publish_state already-current "$version" "$commit_sha" "$release_ref"
    return
  fi
  if [ "$remote_ref_exists" = true ]; then
    fail "$release_ref was concurrently created at $remote_ref_commit; refusing to move it to $commit_sha"
  fi
  fail "failed to create $release_ref and the ref remains absent"
}

if [ "$#" -ne 1 ]; then
  fail "usage: scripts/ci/core-release-ref.sh <resolve|publish>"
fi

case "$1" in
  resolve)
    resolve_candidate
    ;;
  publish)
    publish_candidate
    ;;
  *)
    fail "unknown operation: $1"
    ;;
esac
