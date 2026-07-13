#!/usr/bin/env bash
# CI-parity browser gate (Linux). Mirrors ci.yml web-playwright verbatim:
# direct Playwright invocation is the sanctioned work-plan §10.4 path on Linux
# (web/playwright.config.ts's hard-reject applies only to Windows), followed
# by the D1/D2 WebRTC interop suite through its dedicated config. The harness
# builds the windshare/wsrelay/hostile-sender child binaries itself, so the Go
# toolchain is required alongside node 24 + pnpm.
# Note: `playwright install --with-deps` may prompt for sudo to install system
# libraries; pre-install Chromium once if running unattended.
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== browser =="

echo "-- pnpm install (frozen lockfile)"
pnpm -C web install --frozen-lockfile

echo "-- install Chromium"
pnpm -C web exec playwright install --with-deps chromium

echo "-- Playwright M1b+M1c suite"
pnpm -C web exec playwright test

echo "-- Playwright D1/D2 WebRTC interop suite"
pnpm -C web exec playwright test --config test/transport/webrtc/browser.playwright.config.ts

echo "== browser: PASS in ${SECONDS}s =="
