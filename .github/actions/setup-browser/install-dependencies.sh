#!/usr/bin/env bash
set -euo pipefail

readonly APT_ATTEMPT_TIMEOUT_SECONDS=120
readonly APT_IDLE_TIMEOUT_SECONDS=30
readonly APT_DOWNLOAD_RETRIES=1
readonly PROCESS_KILL_GRACE_SECONDS=10
readonly DPKG_RECOVERY_TIMEOUT_SECONDS=30
readonly APT_DIRECTORY=/etc/apt
readonly AZURE_UBUNTU_MIRROR_PATTERN='(https?://)azure\.archive\.ubuntu\.com/ubuntu(/?)([[:space:]]|$)'
readonly UBUNTU_MIRROR_REPLACEMENT='\1archive.ubuntu.com/ubuntu\2\3'

configure_apt_downloads() {
  sudo tee "$APT_DIRECTORY/apt.conf.d/80windshare-browser" >/dev/null <<APT
Acquire::http::Timeout "$APT_IDLE_TIMEOUT_SECONDS";
Acquire::https::Timeout "$APT_IDLE_TIMEOUT_SECONDS";
Acquire::Retries "$APT_DOWNLOAD_RETRIES";
APT
}

use_fallback_ubuntu_mirror() {
  local apt_directory="$1" source
  local sources=()
  # Match only the runner's Ubuntu archive; preserve suites, signing keys,
  # security updates and third-party repositories in both APT source formats.
  for source in "$apt_directory/sources.list" "$apt_directory"/sources.list.d/*.list "$apt_directory"/sources.list.d/*.sources; do
    if [[ -f "$source" ]] && grep -Eq "$AZURE_UBUNTU_MIRROR_PATTERN" "$source"; then
      sources+=("$source")
    fi
  done
  if (( ${#sources[@]} == 0 )); then
    echo 'browser-setup phase=mirror-fallback outcome=unavailable'
    return 1
  fi
  sudo sed -E -i "s#$AZURE_UBUNTU_MIRROR_PATTERN#$UBUNTU_MIRROR_REPLACEMENT#g" "${sources[@]}" || return $?
  echo 'browser-setup phase=mirror-fallback outcome=selected mirror=archive.ubuntu.com'
}

run_bounded_dependency_command() {
  local phase="$1" budget_seconds="$2" started_at="$SECONDS" status=0
  shift 2
  printf 'browser-setup phase=%s event=start budget_seconds=%s\n' "$phase" "$budget_seconds"
  # Timeout runs as root so it can terminate the entire apt/dpkg process group.
  # An idle socket timeout alone cannot bound a mirror that keeps trickling data.
  sudo timeout --kill-after="${PROCESS_KILL_GRACE_SECONDS}s" "${budget_seconds}s" \
    env "PATH=$PATH" DEBIAN_FRONTEND=noninteractive "$@" || status=$?
  printf 'browser-setup phase=%s event=finish exit_code=%s elapsed_seconds=%s\n' "$phase" "$status" "$((SECONDS - started_at))"
  return "$status"
}

install_browser_dependencies() {
  if (( $# == 0 )); then
    echo 'At least one Playwright browser is required' >&2
    return 2
  fi
  configure_apt_downloads || return $?
  local primary_status
  if run_bounded_dependency_command primary "$APT_ATTEMPT_TIMEOUT_SECONDS" \
    pnpm -C web exec playwright install-deps "$@"; then
    return 0
  else
    primary_status=$?
  fi
  # A retry is useful only after changing the failed download source.
  use_fallback_ubuntu_mirror "$APT_DIRECTORY" || return "$primary_status"
  # A total-time budget can interrupt package configuration as well as downloads.
  # Repair that transaction before asking Playwright to install the same package set.
  run_bounded_dependency_command recovery "$DPKG_RECOVERY_TIMEOUT_SECONDS" \
    dpkg --configure -a || return $?
  run_bounded_dependency_command fallback "$APT_ATTEMPT_TIMEOUT_SECONDS" \
    pnpm -C web exec playwright install-deps "$@"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  install_browser_dependencies "$@"
fi
