#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== web =="

echo "-- ESLint"
pnpm -C web lint

echo "-- TypeScript and Vite build"
pnpm -C web build

echo "-- Vitest"
pnpm -C web test

echo "== web: PASS in ${SECONDS}s =="
