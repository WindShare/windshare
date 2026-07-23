# Local CI mirror — thin entry point only. Every target dispatches to a
# platform gate script; all gate logic lives in those scripts, none here:
#   Windows: pwsh -NoProfile -File scripts/ci/<target>.ps1
#   Linux:   bash scripts/ci/<target>.sh
#
# Usage: `make ci` runs every gate of .github/workflows/ci.yml in
# substance-first order (owner decision 2026-07-14): gates that catch
# compile/runtime errors (vet core-release race vectors coverage network web
# browser) run before the style/hygiene gates (hygiene lint), with sloc last — an
# iterating agent sees real failures before style noise. Each gate is also
# independently invokable (e.g. `make race`).
# Expected full `make ci` runtime on Windows: budget ~10 minutes warm and
# ~30 minutes cold, including the extracted core release sweep.
# The core artifact invariant runs in `core-release`; the root GOWORK=off
# consumer build remains in `vet`.
#
# Fidelity note: nothing is deduplicated or excluded relative to CI —
# fidelity to CI beats local speed. Per-gate CI-job parity is recorded in
# docs/.orchestration/m1/make-ci.md.

GATES := vet core-release race vectors coverage network web browser hygiene lint sloc

ifeq ($(OS),Windows_NT)
DISPATCH = pwsh -NoProfile -File scripts/ci/$@.ps1
else
DISPATCH = bash scripts/ci/$@.sh
endif

# Gates share the worktree, the Go build cache and the exclusive D5 harness
# lease, so parallel `make -j` would only interleave failures.
.NOTPARALLEL:

.PHONY: ci $(GATES)

ci: $(GATES)
	@echo "ci: all gates passed"

$(GATES):
	$(DISPATCH)
