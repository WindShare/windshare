#!/usr/bin/env bash
# CI-parity web gate (Linux). Mirrors ci.yml `web` step for step: frozen
# install, lint, forced typecheck, build, the v1-forbidden production graph,
# and vitest (which consumes every retained golden-vector family). Prerequisites: node 24 + pnpm (version pinned by the
# packageManager field in web/package.json).
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== web =="

echo "-- pnpm install (frozen lockfile)"
pnpm -C web install --frozen-lockfile

echo "-- pnpm lint"
pnpm -C web lint

echo "-- forced typecheck (tsc -b --force)"
pnpm -C web exec tsc -b --force

echo "-- pnpm build"
pnpm -C web build

echo "-- v1 forbidden production graph and bundle"
pnpm -C web forbidden

echo "-- vitest (consumes all golden-vector families)"
pnpm -C web test

echo "== web: PASS in ${SECONDS}s =="
