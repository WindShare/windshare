#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/install-dependencies.sh"

fail() {
  echo "browser setup contract: $*" >&2
  exit 1
}

run_recovery_case() (
  local label="$1" simulated_primary_exit="$2" simulated_recovery_exit="$3" simulated_fallback_exit="$4"
  local mirror_status="$5" configure_status="$6" expected_status="$7" expected_events="$8"
  local install_count=0 status=0
  local events=()

  configure_apt_downloads() {
    events+=(configure)
    return "$configure_status"
  }
  use_fallback_ubuntu_mirror() {
    events+=(mirror)
    return "$mirror_status"
  }
  sudo() {
    [[ "$1" == timeout && "$2" == "--kill-after=${PROCESS_KILL_GRACE_SECONDS}s" ]] || fail 'unbounded privileged command'
    local budget="$3"
    shift 3
    [[ "$1" == env && "$2" == "PATH=$PATH" && "$3" == DEBIAN_FRONTEND=noninteractive ]] || fail 'lost package environment'
    shift 3
    case "$1" in
      pnpm)
        [[ "$budget" == "${APT_ATTEMPT_TIMEOUT_SECONDS}s" ]] || fail 'unbounded dependency attempt'
        [[ "$*" == 'pnpm -C web exec playwright install-deps chromium webkit' ]] || fail 'changed browser selection'
        events+=(install)
        install_count=$((install_count + 1))
        if (( install_count == 1 )); then return "$simulated_primary_exit"; fi
        (( install_count == 2 )) || fail 'retried more than once'
        return "$simulated_fallback_exit"
        ;;
      dpkg)
        [[ "$budget" == "${DPKG_RECOVERY_TIMEOUT_SECONDS}s" ]] || fail 'unbounded package recovery'
        [[ "$*" == 'dpkg --configure -a' ]] || fail 'incorrect interrupted-package recovery'
        events+=(recovery)
        return "$simulated_recovery_exit"
        ;;
      *) fail "unexpected privileged command: $*" ;;
    esac
  }

  install_browser_dependencies chromium webkit || status=$?
  [[ "$status" == "$expected_status" ]] || fail "$label: expected exit $expected_status, got $status"
  [[ "${events[*]}" == "$expected_events" ]] || fail "$label: unexpected events: ${events[*]}"
  printf 'PASS: %s\n' "$label"
)

run_recovery_case 'healthy mirror needs no fallback' 0 0 0 0 0 0 'configure install'
run_recovery_case 'slow mirror switches after total timeout' 124 0 0 0 0 0 'configure install mirror recovery install'
run_recovery_case 'forced termination repairs package state' 137 0 0 0 0 0 'configure install mirror recovery install'
run_recovery_case 'failed download switches mirror' 100 0 0 0 0 0 'configure install mirror recovery install'
run_recovery_case 'unknown mirror preserves primary failure' 124 0 0 1 0 124 'configure install mirror'
run_recovery_case 'failed package recovery stops installation' 124 42 0 0 0 42 'configure install mirror recovery'
run_recovery_case 'fallback failure is terminal' 124 0 100 0 0 100 'configure install mirror recovery install'
run_recovery_case 'failed APT configuration stops installation' 0 0 0 0 13 13 'configure'

(
  configure_apt_downloads() { fail 'empty browser selection touched APT'; }
  status=0
  install_browser_dependencies || status=$?
  [[ "$status" == 2 ]] || fail 'empty browser selection was accepted'
  echo 'PASS: empty browser selection fails before changing the runner'
)

(
  temporary_parent="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
  fixture_root="$(mktemp -d "$temporary_parent/windshare-browser-setup.XXXXXXXX")"
  cleanup() {
    local status=$?
    case "$fixture_root" in
      "$temporary_parent"/windshare-browser-setup.*) rm -rf -- "$fixture_root" ;;
      *) echo "refusing to remove unowned fixture: $fixture_root" >&2; status=1 ;;
    esac
    exit "$status"
  }
  trap cleanup EXIT
  sudo() { "$@"; }
  mkdir -p "$fixture_root/sources.list.d"
  cat >"$fixture_root/sources.list" <<'SOURCES'
deb [signed-by=/keys/ubuntu.gpg] https://azure.archive.ubuntu.com/ubuntu/ noble main
deb http://security.ubuntu.com/ubuntu noble-security main
SOURCES
  cat >"$fixture_root/sources.list.d/ubuntu.sources" <<'SOURCES'
Types: deb
URIs: http://azure.archive.ubuntu.com/ubuntu
Suites: noble noble-updates
Components: main universe
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
SOURCES
  cat >"$fixture_root/sources.list.d/vendor.list" <<'SOURCES'
deb https://packages.microsoft.com/repos/azure-cli/ noble main
deb https://azure.archive.ubuntu.com.example/ubuntu noble main
SOURCES
  cp "$fixture_root/sources.list.d/vendor.list" "$fixture_root/vendor.before"
  use_fallback_ubuntu_mirror "$fixture_root"
  diff -u "$fixture_root/sources.list" - <<'SOURCES'
deb [signed-by=/keys/ubuntu.gpg] https://archive.ubuntu.com/ubuntu/ noble main
deb http://security.ubuntu.com/ubuntu noble-security main
SOURCES
  diff -u "$fixture_root/sources.list.d/ubuntu.sources" - <<'SOURCES'
Types: deb
URIs: http://archive.ubuntu.com/ubuntu
Suites: noble noble-updates
Components: main universe
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
SOURCES
  diff -u "$fixture_root/vendor.before" "$fixture_root/sources.list.d/vendor.list"
  if use_fallback_ubuntu_mirror "$fixture_root"; then fail 'same mirror can be retried indefinitely'; fi
  echo 'PASS: both APT formats preserve repository identity and unrelated sources'
)
