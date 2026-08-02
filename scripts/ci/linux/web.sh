#!/usr/bin/env bash
# CI-parity web gate (Linux). Dependency acquisition is a shared Make leaf;
# this gate owns lint, forced typecheck, build, the v1-forbidden production
# graph, and vitest (which consumes every retained golden-vector family).
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== web =="

echo "-- pnpm lint"
pnpm -C web lint

echo "-- forced typecheck (tsc -b --force)"
pnpm -C web exec tsc -b --force

echo "-- pnpm build"
pnpm -C web build

echo "-- v1 forbidden production graph and bundle"
pnpm -C web forbidden

echo "-- vitest remainder (browser contracts have one dedicated owner)"
pnpm -C web run test:unit:remainder

echo "== web: PASS in ${SECONDS}s =="
