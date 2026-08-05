#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
readonly FIREFOX_WEBKIT_CONTRACT_PORT=4198
readonly CHROMIUM_PERIODIC_CONTRACT_PORT=4199
echo "== browser-weekly-supplement =="

echo "-- Firefox and WebKit component contracts"
WINDSHARE_CONTRACT_PORT="$FIREFOX_WEBKIT_CONTRACT_PORT" pnpm -C web run test:browser:contract:cross

echo "-- Chromium periodic component contracts"
WINDSHARE_CONTRACT_PORT="$CHROMIUM_PERIODIC_CONTRACT_PORT" pnpm -C web run test:browser:contract:periodic

echo "-- progressive catalog paging"
pnpm -C web run test:browser:progressive

echo "-- direct and TURN relay-cut routes"
pnpm -C web run test:browser:network

echo "-- browser and Pion interoperability"
pnpm -C web run test:browser:interop

echo "-- Firefox and WebKit product routes"
pnpm -C web run test:browser:cross

echo "== browser-weekly-supplement: PASS in ${SECONDS}s =="
