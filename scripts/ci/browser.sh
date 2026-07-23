#!/usr/bin/env bash
# CI-parity browser gate (Linux). Mirrors ci.yml web-playwright verbatim:
# direct Playwright invocation is the sanctioned work-plan §10.4 path on Linux
# (web/playwright.config.ts's hard-reject applies only to Windows), followed
# by the production Pion WebRTC matrix through its dedicated config. The harness
# builds the windshare/wsrelay child binaries itself, so the Go
# toolchain is required alongside node 24 + pnpm.
# Note: `playwright install --with-deps` may prompt for sudo to install system
# libraries; pre-install the three-engine matrix once if running unattended.
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== browser =="

echo "-- pnpm install (frozen lockfile)"
pnpm -C web install --frozen-lockfile

echo "-- install Chromium/Firefox/WebKit"
pnpm -C web exec playwright install --with-deps chromium firefox webkit

echo "-- verify pinned browser executables"
pnpm -C web run test:browser:preflight

echo "-- Playwright three-engine scenario suite"
pnpm -C web exec playwright test

echo "-- Playwright production Pion WebRTC browser matrix"
pnpm -C web exec playwright test --config test/transport/webrtc/browser.playwright.config.ts

echo "== browser: PASS in ${SECONDS}s =="
