#!/usr/bin/env bash
# Deterministic root module release gate (POSIX). The gate reads one exact commit
# object, extracts its complete source bundle outside the repository, and validates
# it without workspace or worktree state. Linux release evidence ends with an
# isolated loop-ext4 fixture, where the production receiver runs unprivileged and
# must prove deterministic inode-reuse rejection.
set -euo pipefail
cd "$(dirname "$0")/../../.."

repository_root="$(pwd -P)"
if ! command -v go >/dev/null 2>&1; then
  echo "release requires the local Go toolchain on PATH" >&2
  exit 1
fi
if ! govulncheck_executable="$(command -v govulncheck)" ||
  [ ! -x "$govulncheck_executable" ]; then
  echo "release requires developer-installed govulncheck on PATH" >&2
  exit 1
fi
# Native certification needs the same coordinator toolchain path; publish the
# locally resolved Go command without retaining it.
WINDSHARE_GO_EXECUTABLE="$(command -v go)"
export WINDSHARE_GO_EXECUTABLE
source scripts/ci/release-checkout.sh
if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "usage: scripts/ci/linux/release.sh <version> <commit-sha> [linux-ext4]" >&2
  exit 2
fi
release_version="$1"
release_commit="$2"
native_profile="${3:-}"
if [[ ! "$release_commit" =~ ^[0-9a-f]{40}$ ]]; then
  echo "release commit must be an exact lowercase 40-character SHA" >&2
  exit 2
fi
host_kernel="$(uname -s)"
if [ "$host_kernel" = "Linux" ] && [ -z "$native_profile" ]; then
  # A Linux release sweep is certification evidence, so silently omitting the
  # native profile would make the local gate weaker than the hosted release.
  native_profile="linux-ext4"
fi
if [ "$native_profile" != "" ] && [ "$native_profile" != "linux-ext4" ]; then
  echo "unsupported POSIX release native profile: $native_profile" >&2
  exit 2
fi
if [ "$native_profile" = "linux-ext4" ] && [ "$host_kernel" != "Linux" ]; then
  echo "the linux-ext4 release profile requires a Linux host" >&2
  exit 2
fi
unset WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION
unset WINDSHARE_LINUX_NATIVE_FIXTURE
unset WINDSHARE_LINUX_NATIVE_TEMP_ROOT
unset WINDSHARE_LINUX_NATIVE_REUSE_ROOT

# The extracted osfs suite legitimately approaches Go's 10-minute default on
# hosted runners. The workflow job remains the outer hang bound; this package
# timeout prevents cumulative suite work from being mistaken for a stuck test.
module_suite_test_timeout="30m"

windshare_assert_exact_release_checkout "$repository_root" "$release_commit"
echo "-- exact release checkout contract"
bash scripts/ci/release-checkout.tests.sh
linux_release_environment=false
if [ "$host_kernel" = "Linux" ]; then
  echo "-- release-ref resolver contract"
  bash scripts/ci/release-ref.tests.sh
  echo "-- Linux private-temp helper contract"
  bash scripts/ci/native-output/linux/environment.tests.sh
  source scripts/ci/native-output/linux/environment.sh
  windshare_linux_prepare_release_environment "$repository_root"
  temporary_base="$WINDSHARE_LINUX_RELEASE_TEMP_PARENT"
  temporary_root="$WINDSHARE_LINUX_RELEASE_TEMP_ROOT"
  linux_release_environment=true
else
  temporary_base="${TMPDIR:-/tmp}"
  temporary_base="${temporary_base%/}"
  if [ -z "$temporary_base" ]; then
    temporary_base="/"
  fi
  temporary_root="$(mktemp -d "$temporary_base/windshare-release.XXXXXXXX")"
fi
stage_directory="$temporary_root/committed-module"
zip_path="$temporary_root/source.zip"
artifact_root="$temporary_root/extracted-module"
release_repository="$temporary_root/release-repository"

cleanup() {
  local exit_status="$?"
  local cleanup_status
  trap - EXIT
  set +e
  if [ "$linux_release_environment" = true ]; then
    windshare_linux_cleanup_release_environment
    cleanup_status="$?"
  else
    case "$temporary_root" in
      "$temporary_base"/windshare-release.*)
        rm -rf -- "$temporary_root"
        cleanup_status="$?"
        ;;
      *)
        echo "refusing to remove unowned temporary path: $temporary_root" >&2
        cleanup_status=1
        ;;
    esac
  fi
  if [ "$exit_status" -eq 0 ] && [ "$cleanup_status" -ne 0 ]; then
    exit_status="$cleanup_status"
  fi
  exit "$exit_status"
}
trap cleanup EXIT

SECONDS=0
echo "== release =="

source scripts/ci/release-environment.sh
windshare_prepare_release_go_environment "$temporary_root"

echo "-- private Go environment contract"
bash scripts/ci/release-environment.tests.sh
echo "-- commit-bound archive contract"
bash scripts/ci/release-archive.tests.sh

echo "-- GOWORK=off go vet release helper"
go vet ./scripts/ci/_sourcebundle
echo "-- GOWORK=off go test release helper"
go test -count=1 ./scripts/ci/_sourcebundle ./scripts/ci/_releaseassets
# Helper tests are allowed to execute code, so re-prove the verifier checkout
# before compiling the archive builder that supplies release evidence.
windshare_assert_exact_release_checkout "$repository_root" "$release_commit"

echo "-- materialize private exact-commit verifier checkout"
windshare_create_exact_release_checkout \
  "$repository_root" \
  "$release_commit" \
  "$release_repository" \
  "${WINDSHARE_RELEASE_VERIFIER_PATHS[@]}"

echo "-- construct deterministic source bundle ($release_version at $release_commit)"
(
  cd "$release_repository"
  go run ./scripts/ci/_sourcebundle \
    -repo "$release_repository" \
    -commit "$release_commit" \
    -stage "$stage_directory" \
    -zip "$zip_path" \
    -extract "$artifact_root" \
    -version "$release_version"
)

case "$artifact_root" in
  "$repository_root"/*)
    echo "extracted root module artifact must live outside the repository" >&2
    exit 1
    ;;
esac
if [ -e "$artifact_root/go.work" ]; then
  echo "root module artifact must not contain go.work" >&2
  exit 1
fi

(
  cd "$artifact_root"
  export GOWORK=off

  echo "-- pinned provider source and patch reproduction (source bundle)"
  go run ./scripts/ci/_piondeps -reproduce
  echo "-- GOWORK=off go mod tidy -diff (extracted module)"
  go mod tidy -diff
  echo "-- GOWORK=off go mod verify (extracted module)"
  go mod verify
  echo "-- GOWORK=off go list ./... (extracted module)"
  go list ./...
  echo "-- core production dependency boundary (extracted module)"
  go run ./scripts/ci/_coreboundary
  echo "-- GOWORK=off go vet ./... (extracted module)"
  go vet ./...
  echo "-- GOWORK=off go build ./... (extracted module)"
  go build ./...
  echo "-- govulncheck (extracted module)"
  # The setup boundary owns scanner upgrades; the repository depends only on
  # govulncheck's stable source-scan package-pattern contract.
  "$govulncheck_executable" ./...

  echo "-- install wind CLI from the extracted release revision"
  install_root="$temporary_root/installed-cli"
  install -d -m 0700 -- "$install_root"
  bash scripts/install/install.sh "$install_root"
  test -x "$install_root/wind"
  "$install_root/wind" --help >/dev/null
)

if [ "$native_profile" = "linux-ext4" ]; then
  windshare_assert_exact_release_file_projection \
    "$release_repository" \
    "$release_commit" \
    scripts/ci/native-output/linux/certify.sh \
    scripts/ci/native-output/linux/root-worker.sh
  bash "$release_repository/scripts/ci/native-output/linux/certify.sh" \
    "$artifact_root" \
    "$temporary_root"
fi

# Package verified source bytes before tests can mutate them.
assets_root="${WINDSHARE_RELEASE_ASSETS:-$temporary_root/assets}"
(
  cd "$release_repository"
  go run ./scripts/ci/_releaseassets -source "$artifact_root" -source-zip "$zip_path" \
    -out "$assets_root" -version "$release_version" -commit "$release_commit"
)

# Module tests execute arbitrary repository code and can mutate their source tree.
# Keeping them last prevents later consumers from silently validating changed bytes.
(
  cd "$artifact_root"
  export GOWORK=off
  echo "-- GOWORK=off go test ./... (extracted module)"
  go test -count=1 -timeout="$module_suite_test_timeout" ./...
)

echo "== release: PASS in ${SECONDS}s =="
