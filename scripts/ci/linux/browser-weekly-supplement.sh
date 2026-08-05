#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== browser-weekly-supplement =="

echo "-- progressive catalog paging"
pnpm -C web run test:browser:progressive

echo "-- direct and TURN relay-cut routes"
pnpm -C web run test:browser:network

echo "-- browser and Pion interoperability"
pnpm -C web run test:browser:interop

echo "-- Firefox and WebKit product smoke"
pnpm -C web run test:browser:cross

echo "== browser-weekly-supplement: PASS in ${SECONDS}s =="
