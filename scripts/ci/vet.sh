#!/usr/bin/env bash
# CI-parity vet gate (Linux). Mirrors:
#  - the vet analysis of ci.yml go-root and go-core (native GOOS=linux — the
#    ubuntu analysis path of work-plan §10.2).
#  - ci.yml windows-tests' vet steps via a GOOS=windows cross-vet of both
#    modules, so Windows-tagged files are analyzed, not just compiled.
#  - ci.yml gowork-off-core / gowork-off-root: the two-module release
#    invariant builds, core first because it is CI's hard gate. They live
#    inside `vet` instead of a separate make target because they are the same
#    cheap compile-class checks and always run together with it in CI.
#
# The plain same-GOOS `go build ./...` steps (root + core) are intentionally
# absent: `go vet` already compiles every package for analysis, the race and
# coverage gates recompile the identical code so any compile break surfaces
# there, and main-package linking is exercised by the D5 stable-children
# builds. Repeating a same-GOOS build here would be pure duplication; only the
# cross-GOOS vet and the GOWORK=off release builds below cover ground those
# gates cannot.
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
echo "== vet =="

echo "-- go vet (root, GOOS=linux)"
go vet ./...

echo "-- go vet (core, GOOS=linux)"
go -C core vet ./...

echo "-- GOOS=windows cross-vet (mirrors ci.yml windows-tests vet)"
GOOS=windows go vet ./...
GOOS=windows go -C core vet ./...

echo "-- GOWORK=off release-invariant builds (core first: CI hard gate)"
GOWORK=off go -C core build ./...
GOWORK=off go build ./...

echo "== vet: PASS in ${SECONDS}s =="
