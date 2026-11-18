#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== web =="

echo "-- Web ESLint"
pnpm -C web lint

echo "-- Consoles ESLint"
pnpm -C consoles lint

echo "-- Consoles TypeScript"
pnpm -C consoles typecheck

echo "-- TypeScript and Vite build"
pnpm -C web build

echo "-- Vitest"
pnpm -C web test

echo "== web: PASS in ${SECONDS}s =="
