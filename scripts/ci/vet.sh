#!/usr/bin/env bash
# CI-parity vet/build gate (Linux). Mirrors:
#  - the vet + build steps of ci.yml go-root and go-core (native GOOS=linux —
#    the ubuntu analysis path of work-plan §10.2).
#  - ci.yml windows-tests' vet steps via a GOOS=windows cross-vet of both
#    modules, so Windows-tagged files are analyzed, not just compiled.
#  - ci.yml gowork-off-core / gowork-off-root: the two-module release
#    invariant builds, core first because it is CI's hard gate. They live
#    inside `vet` instead of a separate make target because they are the same
#    cheap compile-class checks and always run together with it in CI.
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== vet =="

echo "-- go vet/build (root, GOOS=linux)"
go vet ./...
go build ./...

echo "-- go vet/build (core, GOOS=linux)"
go -C core vet ./...
go -C core build ./...

echo "-- GOOS=windows cross-vet (mirrors ci.yml windows-tests vet)"
GOOS=windows go vet ./...
GOOS=windows go -C core vet ./...

echo "-- GOWORK=off release-invariant builds (core first: CI hard gate)"
GOWORK=off go -C core build ./...
GOWORK=off go build ./...

echo "== vet: PASS in ${SECONDS}s =="
