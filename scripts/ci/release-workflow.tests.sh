#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

fail() {
  echo "release workflow contract: $1" >&2
  exit 1
}

assert_contains() {
  local file_path="$1"
  local expected="$2"
  grep -Fq -- "$expected" "$file_path" ||
    fail "$file_path is missing: $expected"
}

assert_not_contains() {
  local file_path="$1"
  local forbidden="$2"
  if grep -Fq -- "$forbidden" "$file_path"; then
    fail "$file_path contains retired release text: $forbidden"
  fi
}

assert_precedes() {
  local file_path="$1"
  local earlier="$2"
  local later="$3"
  local earlier_line
  local later_line
  earlier_line="$(grep -Fn -- "$earlier" "$file_path" | tail -n 1 | cut -d: -f1 || true)"
  later_line="$(grep -Fn -- "$later" "$file_path" | tail -n 1 | cut -d: -f1 || true)"
  [ -n "$earlier_line" ] || fail "$file_path is missing ordered step: $earlier"
  [ -n "$later_line" ] || fail "$file_path is missing ordered step: $later"
  [ "$earlier_line" -lt "$later_line" ] ||
    fail "$file_path runs '$earlier' after '$later'"
}

workflow=.github/workflows/release.yml
weekly=.github/workflows/weekly.yml
ci=.github/workflows/ci.yml
linux_gate=scripts/ci/linux/release.sh
windows_gate=scripts/ci/windows/release.ps1

assert_contains "$workflow" 'workflow_dispatch:'
assert_contains "$workflow" 'commit_sha:'
assert_contains "$workflow" 'version:'
assert_contains "$workflow" 'ref: ${{ github.event.repository.default_branch }}'
assert_contains "$workflow" 'run: bash scripts/ci/release-ref.sh resolve'
assert_contains "$workflow" 'run: bash scripts/ci/release-ref.sh publish'
assert_contains "$workflow" 'scripts/ci/linux/release.sh "$RELEASE_VERSION" "$RELEASE_COMMIT_SHA" linux-ext4'
assert_contains "$workflow" 'scripts/ci/windows/release.ps1 -Version $env:RELEASE_VERSION -CommitSHA $env:RELEASE_COMMIT_SHA -NativeProfile windows-ntfs'
assert_contains "$workflow" 'group: release-linux-${{ needs.release.outputs.version }}'
assert_contains "$workflow" 'group: release-windows-${{ needs.release.outputs.version }}'
assert_contains "$workflow" 'group: release-publish-${{ needs.release.outputs.version }}'
[ "$(grep -Ec '^[[:space:]]+contents:[[:space:]]+write[[:space:]]*$' "$workflow")" -eq 1 ] ||
  fail "exactly one release job must receive contents: write"

for retired in 'core-candidate/' 'refs/tags/core/' 'core/v*'; do
  assert_not_contains "$workflow" "$retired"
  assert_not_contains scripts/ci/release-ref.sh "$retired"
done
assert_contains scripts/ci/release-ref.sh 'git merge-base --is-ancestor'
assert_contains scripts/ci/release-ref.sh 'release_ref_prefix=refs/tags/'
assert_contains scripts/ci/_modulezip/main.go 'modulePath = "github.com/windshare/windshare"'
assert_contains scripts/ci/_modulezip/main.go '"internal/perfevidence/"'
assert_contains scripts/ci/_modulezip/main.go '"spikes/webrtc/"'

for release_gate in "$linux_gate" "$windows_gate"; do
  assert_contains "$release_gate" 'mod tidy -diff'
  assert_contains "$release_gate" 'mod verify'
  assert_contains "$release_gate" './scripts/ci/_coreboundary'
  assert_contains "$release_gate" 'build ./...'
  assert_contains "$release_gate" 'test'
  assert_contains "$release_gate" 'install ./cmd/wind'
done

assert_contains "$linux_gate" '"$install_root/wind" --help'
assert_not_contains "$linux_gate" '"$install_root/windshare"'
assert_contains "$windows_gate" "Join-Path \$cliInstallRoot 'wind.exe'"
assert_not_contains "$windows_gate" "Join-Path \$cliInstallRoot 'windshare.exe'"

# Tests may execute arbitrary repository code, so exact-artifact consumers must
# finish first and the Linux certifier itself must come from the proven checkout.
assert_contains "$linux_gate" 'bash "$release_repository/scripts/ci/native-output/linux/certify.sh"'
assert_not_contains "$linux_gate" 'bash scripts/ci/native-output/linux/certify.sh'
assert_precedes "$linux_gate" 'GOBIN="$install_root" go install ./cmd/wind' \
  'go test -count=1 -timeout="$module_suite_test_timeout" ./...'
assert_precedes "$linux_gate" 'bash "$release_repository/scripts/ci/native-output/linux/certify.sh"' \
  'go test -count=1 -timeout="$module_suite_test_timeout" ./...'
assert_precedes "$windows_gate" '& $goExecutable install ./cmd/wind' \
  '& $goExecutable test -count=1 "-timeout=$moduleSuiteTestTimeout" ./...'
assert_precedes "$windows_gate" '        Invoke-RequiredWindowsNativeTestsAsStandardUser' \
  '& $goExecutable test -count=1 "-timeout=$moduleSuiteTestTimeout" ./...'

assert_contains "$weekly" 'scripts/ci/linux/release.sh'
assert_contains "$weekly" 'scripts/ci/windows/release.ps1'
assert_not_contains "$ci" 'root-release-graph'
assert_not_contains Makefile 'root-release-graph'
assert_not_contains Makefile 'core-release'

retired_paths=(
  .github/workflows/core-release.yml
  scripts/ci/linux/root-release-graph.sh
  scripts/ci/windows/root-release-graph.ps1
  scripts/ci/core-release-ref.sh
  scripts/ci/_coremodulezip/main.go
)
for retired_path in "${retired_paths[@]}"; do
  [ ! -e "$retired_path" ] || fail "retired release path still exists: $retired_path"
done

echo "release workflow tests: PASS"
