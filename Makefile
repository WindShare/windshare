# Local CI mirror — thin entry point only. Every target dispatches to a
# platform gate script; all gate logic lives in those scripts, none here:
#   Windows: pwsh -NoProfile -File scripts/ci/<target>.ps1
#   Linux:   bash scripts/ci/<target>.sh
#
# Usage: `make ci` runs every gate of .github/workflows/ci.yml in
# cheap-to-expensive order (hygiene vet lint race vectors coverage network
# web browser); each gate is also independently invokable (e.g. `make race`).
# Expected full `make ci` runtime on Windows: ~9 minutes with a warm Go
# build cache (8:40 measured 2026-07-13); budget ~30 minutes cold. The
# GOWORK=off release invariant (ci.yml gowork-off-core / gowork-off-root)
# runs inside the `vet` gate.
#
# Fidelity note: nothing is deduplicated or excluded relative to CI —
# fidelity to CI beats local speed. Per-gate CI-job parity is recorded in
# docs/.orchestration/make-ci.md.

GATES := hygiene vet lint race vectors coverage network web browser

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
