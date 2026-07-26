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

# Keeping the candidate at the caller makes a version bump visible; the release
# scripts deliberately have no environment- or tag-derived version fallback.
# The archive source is an explicit commit object; live index/worktree bytes are
# never prospective publication evidence.
# The POSIX entry point itself makes linux-ext4 mandatory on Linux so this thin
# dispatcher cannot accidentally select a skippable release sweep.
CORE_RELEASE_VERSION ?= v0.3.0
CORE_RELEASE_COMMIT_SHA ?= $(shell git rev-parse --verify HEAD)
GATES := vet core-release race vectors coverage network web browser hygiene lint sloc
SCRIPT_GATES := $(filter-out core-release,$(GATES))

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

core-release:
ifeq ($(OS),Windows_NT)
	pwsh -NoProfile -File scripts/ci/core-release.ps1 -Version $(CORE_RELEASE_VERSION) -CommitSHA $(CORE_RELEASE_COMMIT_SHA)
else
	bash scripts/ci/core-release.sh $(CORE_RELEASE_VERSION) $(CORE_RELEASE_COMMIT_SHA)
endif

$(SCRIPT_GATES):
	$(DISPATCH)
