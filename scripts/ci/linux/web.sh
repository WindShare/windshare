#!/usr/bin/env bash
# CI-parity web gate (Linux). Dependency acquisition is a shared Make leaf;
# `pnpm build` owns the single TypeScript compilation before Vite bundles the
# application, avoiding a second forced project build in this wrapper.
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== web =="

echo "-- pnpm lint"
pnpm -C web lint

echo "-- pnpm build (single TypeScript compile and Vite bundle)"
pnpm -C web build

echo "-- v1 forbidden production graph and bundle"
pnpm -C web forbidden

echo "-- vitest remainder (browser contracts have one dedicated owner)"
pnpm -C web run test:unit:remainder

echo "== web: PASS in ${SECONDS}s =="
