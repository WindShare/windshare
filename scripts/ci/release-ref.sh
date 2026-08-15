#!/usr/bin/env bash
set -euo pipefail

readonly release_ref_prefix=refs/tags/

fail() {
  echo "release ref: $1" >&2
  exit 1
}

validate_sha() {
  if [[ ! "${1:-}" =~ ^[0-9a-f]{40}$ ]]; then
    fail "release commit must be an exact lowercase 40-character SHA"
  fi
}

validate_version() {
  if [[ ! "${1:-}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    fail "release version must have the form vX.Y.Z without leading zeroes: ${1:-}"
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

require_default_branch_reachability() {
  local commit_sha="$1"
  local default_branch="${DEFAULT_BRANCH:-}"
  local remote_name="${RELEASE_REMOTE:-origin}"
  local default_ref

  if [ -z "$default_branch" ] ||
     ! git check-ref-format "refs/heads/$default_branch" >/dev/null 2>&1; then
    fail "DEFAULT_BRANCH must name a valid branch"
  fi
  if ! git remote get-url "$remote_name" >/dev/null 2>&1; then
    fail "release remote does not exist: $remote_name"
  fi
  default_ref="refs/remotes/$remote_name/$default_branch"
  if ! git rev-parse --verify "$default_ref^{commit}" >/dev/null 2>&1; then
    fail "default branch is unavailable locally: $default_ref"
  fi
  if ! git merge-base --is-ancestor "$commit_sha" "$default_ref"; then
    fail "release commit must be reachable from $remote_name/$default_branch"
  fi
}

resolve_release() {
  local commit_sha="${INPUT_COMMIT_SHA:-}"
  local version="${INPUT_VERSION:-}"
  local remote_name="${RELEASE_REMOTE:-origin}"
  local release_ref

  validate_sha "$commit_sha"
  validate_version "$version"
  require_commit_object "$commit_sha" "release commit"
  require_default_branch_reachability "$commit_sha"
  release_ref="$release_ref_prefix$version"
  read_remote_release_ref "$remote_name" "$release_ref"
  if [ "$remote_ref_exists" = true ] && [ "$remote_ref_commit" != "$commit_sha" ]; then
    fail "$release_ref already points to $remote_ref_commit; refusing to validate a move to $commit_sha"
  fi
  if [ -z "${GITHUB_OUTPUT:-}" ]; then
    fail "GITHUB_OUTPUT is required"
  fi

  printf 'release resolve: version=%s commit=%s authority=%s/%s\n' \
    "$version" "$commit_sha" "${RELEASE_REMOTE:-origin}" "$DEFAULT_BRANCH"
  printf 'commit_sha=%s\nversion=%s\n' "$commit_sha" "$version" >>"$GITHUB_OUTPUT"
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
  remote_ref_commit="${peeled_commit:-$direct_object}"
}

log_publish_state() {
  local state="$1"
  local version="$2"
  local commit_sha="$3"
  local release_ref="$4"
  printf 'release publish: state=%s version=%s commit=%s ref=%s\n' \
    "$state" "$version" "$commit_sha" "$release_ref"
}

publish_release() {
  local version="${RELEASE_VERSION:-}"
  local commit_sha="${RELEASE_COMMIT_SHA:-}"
  local remote_name="${RELEASE_REMOTE:-origin}"
  local release_ref

  validate_version "$version"
  validate_sha "$commit_sha"
  require_commit_object "$commit_sha" "release commit"
  require_default_branch_reachability "$commit_sha"

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
    fail "$release_ref was not created at the release commit"
  fi

  # A competing idempotent publisher may win between the read and create.
  # Re-reading accepts only the same immutable result and never forces a tag.
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
  fail "usage: scripts/ci/release-ref.sh <resolve|publish>"
fi

case "$1" in
  resolve)
    resolve_release
    ;;
  publish)
    publish_release
    ;;
  *)
    fail "unknown operation: $1"
    ;;
esac
