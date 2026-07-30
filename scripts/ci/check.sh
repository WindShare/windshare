#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

go vet ./...
(cd core && go vet ./...)
pnpm -C web lint
pnpm -C web exec tsc -b --force
pnpm -C web run test:unit:remainder
