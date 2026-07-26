#!/usr/bin/env bash
# Deterministic core module release gate (POSIX). The gate reads core exclusively
# from one exact commit object, extracts its canonical module zip outside the
# repository, and validates it without go.work or parent-module state. Linux
# always enables its native profile so an unsupported environment fails instead
# of turning release certification into a skip.
set -euo pipefail
cd "$(dirname "$0")/../.."

repository_root="$(pwd -P)"
source scripts/ci/core-release-checkout.sh
if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "usage: scripts/ci/core-release.sh <version> <commit-sha> [linux-ext4]" >&2
  exit 2
fi
release_version="$1"
release_commit="$2"
native_profile="${3:-}"
if [[ ! "$release_commit" =~ ^[0-9a-f]{40}$ ]]; then
  echo "core release commit must be an exact lowercase 40-character SHA" >&2
  exit 2
fi
host_kernel="$(uname -s)"
if [ "$host_kernel" = "Linux" ] && [ -z "$native_profile" ]; then
  # A Linux release sweep is certification evidence, so silently omitting the
  # native profile would make the local gate weaker than the hosted release.
  native_profile="linux-ext4"
fi
if [ "$native_profile" != "" ] && [ "$native_profile" != "linux-ext4" ]; then
  echo "unsupported POSIX core-release native profile: $native_profile" >&2
  exit 2
fi
if [ "$native_profile" = "linux-ext4" ] && [ "$host_kernel" != "Linux" ]; then
  echo "the linux-ext4 core-release profile requires a Linux host" >&2
  exit 2
fi
if [ "$native_profile" = "linux-ext4" ]; then
  export WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION="$native_profile"
fi

coverage_tool="github.com/vladopajic/go-test-coverage/v2@v2.18.8"
# The extracted osfs suite legitimately approaches Go's 10-minute default on
# hosted runners. The workflow job remains the outer hang bound; this package
# timeout prevents cumulative suite work from being mistaken for a stuck test.
core_suite_test_timeout="30m"

windshare_assert_exact_release_checkout "$repository_root" "$release_commit"
echo "-- exact release checkout contract"
bash scripts/ci/core-release-checkout.tests.sh
linux_release_environment=false
if [ "$host_kernel" = "Linux" ]; then
  echo "-- release-ref resolver contract"
  bash scripts/ci/core-release-ref.tests.sh
  echo "-- Linux private-temp helper contract"
  bash scripts/ci/core-release-linux-temp.tests.sh
  source scripts/ci/core-release-linux-temp.sh
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
  temporary_root="$(mktemp -d "$temporary_base/windshare-core-release.XXXXXXXX")"
fi
stage_directory="$temporary_root/committed-core"
zip_path="$temporary_root/core.zip"
artifact_root="$temporary_root/extracted-core"
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
      "$temporary_base"/windshare-core-release.*)
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
echo "== core-release =="

source scripts/ci/core-release-environment.sh
windshare_prepare_core_release_go_environment "$temporary_root"

echo "-- private Go environment contract"
bash scripts/ci/core-release-environment.tests.sh
echo "-- commit-bound archive contract"
bash scripts/ci/core-release-archive.tests.sh

echo "-- GOWORK=off go vet release helpers"
go vet ./scripts/ci/_coremodulezip ./scripts/ci/_corevulnerability
echo "-- GOWORK=off go test release helpers"
go test -count=1 ./scripts/ci/_coremodulezip ./scripts/ci/_corevulnerability
echo "-- GOWORK=off go test -race release helpers"
go test -race -count=1 ./scripts/ci/_coremodulezip ./scripts/ci/_corevulnerability

# Helper tests are allowed to execute code, so re-prove the verifier checkout
# before compiling the archive builder that supplies release evidence.
windshare_assert_exact_release_checkout "$repository_root" "$release_commit"

echo "-- materialize private exact-commit verifier checkout"
windshare_create_exact_release_checkout \
  "$repository_root" \
  "$release_commit" \
  "$release_repository" \
  "${WINDSHARE_CORE_RELEASE_VERIFIER_PATHS[@]}"

echo "-- construct deterministic core module zip ($release_version at $release_commit)"
(
  cd "$release_repository"
  go run ./scripts/ci/_coremodulezip/main.go \
    -repo "$release_repository" \
    -commit "$release_commit" \
    -stage "$stage_directory" \
    -zip "$zip_path" \
    -extract "$artifact_root" \
    -version "$release_version"
)

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
  echo "-- GOWORK=off go vet ./... (extracted core)"
  go vet ./...
  echo "-- GOWORK=off go build ./... (extracted core)"
  go build ./...
  echo "-- version-pinned govulncheck (extracted core)"
  (
    windshare_assert_exact_release_file_projection \
      "$release_repository" \
      "$release_commit" \
      go.mod \
      go.sum \
      scripts/ci/_corevulnerability/main.go
    cd "$release_repository"
    GOWORK=off go run ./scripts/ci/_corevulnerability \
      -module "$artifact_root" \
      -cache "$temporary_root/vulnerability-cache"
  )
  echo "-- GOWORK=off go test ./... (extracted core)"
  go test -count=1 -timeout="$core_suite_test_timeout" ./...
  echo "-- GOWORK=off go test -race ./... (extracted core)"
  go test -race -count=1 -timeout="$core_suite_test_timeout" ./...
  echo "-- GOWORK=off go test with coverage (extracted core)"
  go test -count=1 -timeout="$core_suite_test_timeout" ./... -covermode=atomic \
    -coverprofile="$temporary_root/cover.out"
  echo "-- extracted core coverage gate (total >=90%, package >=70%)"
  go run "$coverage_tool" --config=.testcoverage.yml --profile="$temporary_root/cover.out"

  if [ "$native_profile" = "linux-ext4" ]; then
    native_events="$temporary_root/linux-ext4-native-tests.json"
    native_tests='^(TestLinuxExt4NativeCertification|TestLinuxExt4ProcessRestartRecovery)$'
    echo "-- required Linux/ext4 certification and process-restart tests"
    set +e
    TMPDIR="$TMPDIR" WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION="$native_profile" \
      go test -json -count=1 -timeout="$core_suite_test_timeout" \
        -run "$native_tests" ./osfs | tee "$native_events"
    native_status="${PIPESTATUS[0]}"
    set -e
    if [ "$native_status" -ne 0 ]; then
      echo "required Linux/ext4 native tests failed" >&2
      exit "$native_status"
    fi

    # A selected test may hide an environmental skip in a subtest while its
    # top-level test still reports PASS. Certification must fail closed at every
    # level of the selected test tree.
    if grep -Fq '"Action":"skip"' "$native_events"; then
      echo "required Linux/ext4 native test suite reported SKIP" >&2
      grep -F '"Action":"skip"' "$native_events" >&2
      exit 1
    fi

    for required_test in \
      TestLinuxExt4NativeCertification \
      TestLinuxExt4ProcessRestartRecovery; do
      if ! grep -Eq "\"Action\":\"pass\".*\"Test\":\"${required_test}\"" "$native_events"; then
        echo "required native test did not report PASS: $required_test" >&2
        exit 1
      fi
    done
  fi
)

echo "== core-release: PASS in ${SECONDS}s =="
