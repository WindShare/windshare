#!/usr/bin/env bash
# Deterministic core module release gate (POSIX). The gate constructs the
# prospective core tag from tracked files plus non-ignored worktree additions,
# extracts the canonical module zip outside the repository, and validates that
# module without go.work or any parent-module files.
set -euo pipefail
cd "$(dirname "$0")/../.."

repository_root="$(pwd -P)"
if [ -n "${CORE_RELEASE_VERSION:-}" ]; then
  release_version="$CORE_RELEASE_VERSION"
elif [[ "${GITHUB_REF:-}" == refs/tags/core/* ]]; then
  release_version="${GITHUB_REF#refs/tags/core/}"
else
  release_version="v0.2.0"
fi
temporary_base="${TMPDIR:-/tmp}"
temporary_base="${temporary_base%/}"
if [ -z "$temporary_base" ]; then
  temporary_base="/"
fi
temporary_root="$(mktemp -d "$temporary_base/windshare-core-release.XXXXXXXX")"
stage_directory="$temporary_root/projected-core"
zip_path="$temporary_root/core.zip"
artifact_root="$temporary_root/extracted-core"

cleanup() {
  case "$temporary_root" in
    "$temporary_base"/windshare-core-release.*) rm -rf -- "$temporary_root" ;;
    *) echo "refusing to remove unowned temporary path: $temporary_root" >&2 ;;
  esac
}
trap cleanup EXIT

SECONDS=0
echo "== core-release =="

echo "-- construct deterministic core module zip ($release_version)"
GOWORK=off go run ./scripts/ci/_coremodulezip/main.go \
  -repo "$repository_root" \
  -stage "$stage_directory" \
  -zip "$zip_path" \
  -extract "$artifact_root" \
  -version "$release_version"

case "$artifact_root" in
  "$repository_root"/*)
    echo "extracted core artifact must live outside the repository" >&2
    exit 1
    ;;
esac
if [ -e "$artifact_root/go.work" ]; then
  echo "core module artifact must not contain go.work" >&2
  exit 1
fi

(
  cd "$artifact_root"
  export GOWORK=off

  echo "-- GOWORK=off go mod tidy -diff (extracted core)"
  go mod tidy -diff
  echo "-- GOWORK=off go mod verify (extracted core)"
  go mod verify
  echo "-- GOWORK=off go list ./... (extracted core)"
  go list ./...
  echo "-- GOWORK=off go build ./... (extracted core)"
  go build ./...
  echo "-- GOWORK=off go test ./... (extracted core)"
  go test -count=1 ./...
  echo "-- GOWORK=off go test -race ./... (extracted core)"
  go test -race -count=1 ./...
)

echo "== core-release: PASS in ${SECONDS}s =="
