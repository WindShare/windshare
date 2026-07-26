#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

source scripts/ci/core-release-environment.sh

test_parent="${TMPDIR:-/tmp}"
test_parent="${test_parent%/}"
if [ -z "$test_parent" ]; then
  test_parent="/"
fi
test_root="$(mktemp -d "$test_parent/windshare-core-environment.XXXXXXXX")"
fresh_root="$test_root/fresh"
blocked_set_root="$test_root/blocked-set"
blocked_unset_root="$test_root/blocked-unset"

cleanup() {
  local status="$?"
  trap - EXIT
  case "$test_root" in
    "$test_parent"/windshare-core-environment.*) rm -rf -- "$test_root" ;;
    *) echo "refusing to remove unowned environment-test path: $test_root" >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "core release Go environment contract: $1" >&2
  exit 1
}

directory_mode() {
  local directory="$1"

  # GNU and BSD stat expose the same permission value through different flags.
  stat -c '%a' -- "$directory" 2>/dev/null || stat -f '%Lp' "$directory"
}

release_environment_names=(
  GOMODCACHE GOCACHE GOPATH GOENV GOFLAGS GOTOOLCHAIN GOWORK
  GOPROXY GOSUMDB GOPRIVATE GONOSUMDB GONOPROXY GOINSECURE GOTELEMETRY
  GOOS GOARCH CGO_ENABLED GOEXPERIMENT
)

snapshot_release_environment() {
  local variable_name

  for variable_name in "${release_environment_names[@]}"; do
    if ! declare -p "$variable_name" 2>/dev/null; then
      printf 'unset %s\n' "$variable_name"
    fi
  done
}

assert_failed_preflight_preserves_environment() {
  local label="$1"
  local blocked_root="$2"
  local before_environment
  local before_shell_state

  install -d -m 0700 -- "$blocked_root/go-build-cache"
  before_environment="$(env | LC_ALL=C sort)"
  before_shell_state="$(snapshot_release_environment)"
  if windshare_prepare_core_release_go_environment "$blocked_root" >/dev/null 2>&1; then
    fail "$label pre-existing cache did not fail closed"
  fi
  [ ! -e "$blocked_root/go-module-cache" ] || fail "$label failed preflight created a module cache"
  [ ! -e "$blocked_root/go-path" ] || fail "$label failed preflight created a GOPATH"
  [ "$(env | LC_ALL=C sort)" = "$before_environment" ] ||
    fail "$label failed preflight changed the exported environment"
  [ "$(snapshot_release_environment)" = "$before_shell_state" ] ||
    fail "$label failed preflight changed the shell environment state"
}

install -d -m 0700 -- "$fresh_root" "$blocked_set_root" "$blocked_unset_root"
export GOMODCACHE=caller-module GOCACHE=caller-build GOPATH=caller-gopath
export GOENV=caller GOFLAGS=-mod=vendor GOTOOLCHAIN=auto GOWORK=caller.work
export GOPROXY=caller.invalid GOSUMDB=off GOPRIVATE=caller.invalid
export GONOSUMDB=caller.invalid GONOPROXY=caller.invalid GOINSECURE=caller.invalid
export GOTELEMETRY=local
export GOOS=plan9 GOARCH=arm64 CGO_ENABLED=0 GOEXPERIMENT=callerexperiment

windshare_prepare_core_release_go_environment "$fresh_root"

[ "$GOMODCACHE" = "$fresh_root/go-module-cache" ] || fail "GOMODCACHE escaped the owned root"
[ "$GOCACHE" = "$fresh_root/go-build-cache" ] || fail "GOCACHE escaped the owned root"
[ "$GOPATH" = "$fresh_root/go-path" ] || fail "GOPATH escaped the owned root"
for cache_path in "$GOMODCACHE" "$GOCACHE" "$GOPATH"; do
  [ -d "$cache_path" ] && [ ! -L "$cache_path" ] || fail "cache is not a no-follow directory: $cache_path"
  [ "$(directory_mode "$cache_path")" = "700" ] || fail "cache is not mode 0700: $cache_path"
  [ -z "$(find "$cache_path" -mindepth 1 -print -quit)" ] || fail "new cache is not empty: $cache_path"
done

[ "$GOENV" = off ] || fail "GOENV is not off"
[ -z "$GOFLAGS" ] || fail "GOFLAGS is not empty"
[ "$GOTOOLCHAIN" = local ] || fail "GOTOOLCHAIN is not local"
[ "$GOWORK" = off ] || fail "GOWORK is not off"
[ "$GOPROXY" = https://proxy.golang.org ] || fail "GOPROXY is not the public Go proxy"
[ "$GOSUMDB" = sum.golang.org ] || fail "GOSUMDB is not the public checksum database"
for empty_name in GOPRIVATE GONOSUMDB GONOPROXY GOINSECURE; do
  [ -z "${!empty_name}" ] || fail "$empty_name is not empty"
done
[ "$GOTELEMETRY" = off ] || fail "GOTELEMETRY is not off"
for cleared_name in GOOS GOARCH CGO_ENABLED GOEXPERIMENT; do
  if declare -p "$cleared_name" >/dev/null 2>&1; then
    fail "$cleared_name still overrides the host toolchain"
  fi
done

# A failed transaction must preserve values and export attributes. In
# particular, assigning an exported Bash variable keeps it exported while
# assigning an unset name creates a non-exported shell global.
for variable_name in "${release_environment_names[@]}"; do
  printf -v "$variable_name" 'caller-%s' "$variable_name"
  export "$variable_name"
done
assert_failed_preflight_preserves_environment "set caller" "$blocked_set_root"

unset "${release_environment_names[@]}"
assert_failed_preflight_preserves_environment "unset caller" "$blocked_unset_root"

trap - EXIT
rm -rf -- "$test_root"
echo "core release Go environment contract: PASS"
