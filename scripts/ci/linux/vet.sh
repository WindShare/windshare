#!/usr/bin/env bash
# CI-parity vet gate (Linux). Mirrors the current-commit Linux authority:
#  - native GOOS=linux vet analysis of both modules.
#  - the Windows authority via a GOOS=windows cross-vet of both modules, so
#    Windows-tagged files are analyzed, not just compiled.
#  - the released-core consumer build formerly isolated in its own workflow job. The stronger
#    core invariant lives in the separate extracted-artifact `core-release`
#    gate, where no parent repository or go.work can mask a missing file.
#
# The plain same-GOOS `go build ./...` steps (root + core) are intentionally
# absent: `go vet` already compiles every package for analysis, the race and
# coverage gates recompile the identical code so any compile break surfaces
# there, and main-package linking is exercised by the process and E2E fixture
# builds. Repeating a same-GOOS build here would be pure duplication; only the
# cross-GOOS vet and the root GOWORK=off consumer build below cover ground
# those gates cannot.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority

SECONDS=0
echo "== vet =="

echo "-- go vet (root, GOOS=linux)"
windshare_go vet ./...

echo "-- go vet (core, GOOS=linux)"
windshare_go -C core vet ./...

echo "-- GOOS=windows cross-vet (mirrors ci.yml windows-tests vet)"
GOOS=windows windshare_go vet ./...
GOOS=windows windshare_go -C core vet ./...

echo "-- GOWORK=off root released-core consumer build"
GOWORK=off windshare_go build ./...

echo "== vet: PASS in ${SECONDS}s =="
