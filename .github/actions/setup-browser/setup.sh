#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/install-dependencies.sh"

readonly BROWSER_DOWNLOAD_TIMEOUT_SECONDS=180
readonly CHROMIUM_PROBE_TIMEOUT_SECONDS=30

install_headless_browsers() {
  timeout --kill-after="${PROCESS_KILL_GRACE_SECONDS}s" "${BROWSER_DOWNLOAD_TIMEOUT_SECONDS}s" \
    pnpm -C web exec playwright install --only-shell "$@"
}

probe_chromium() (
  cd web || return $?
  local status=0
  timeout --kill-after="${PROCESS_KILL_GRACE_SECONDS}s" "${CHROMIUM_PROBE_TIMEOUT_SECONDS}s" \
    node --input-type=module - <<'JS' || status=$?
import { chromium } from '@playwright/test'
const browser = await chromium.launch()
await browser.close()
JS
  printf 'browser-setup phase=host-probe browser=chromium exit_code=%s\n' "$status"
  return "$status"
)

setup_browsers() {
  # Firefox and WebKit scenarios exercise media support beyond basic startup.
  if (( $# != 1 )) || [[ "$1" != chromium ]]; then
    install_browser_dependencies "$@" || return $?
    install_headless_browsers "$@"
    return
  fi

  # Hosted runner images already include Chrome's libraries. Probe the pinned
  # Playwright shell before downloading another set of system packages.
  install_headless_browsers chromium || return $?
  if probe_chromium; then
    return 0
  fi
  install_browser_dependencies chromium || return $?
  probe_chromium
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  setup_browsers "$@"
fi
