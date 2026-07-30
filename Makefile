# Local CI mirror — thin entry point only. Every target dispatches to a
# platform gate script; all gate logic lives in those scripts, none here:
#   Windows: pwsh -NoProfile -File scripts/ci/<target>.ps1
#   Linux:   bash scripts/ci/<target>.sh
#
# Usage: `make ci` runs every blocking gate in substance-first order, including
# the Docker-free browser gate exactly once.
# Every gate remains independently invokable (e.g. `make race`).
# Expected full `make ci` runtime on Windows: budget ~10 minutes warm and
# ~30 minutes cold, including the extracted core release sweep.
# The core artifact invariant runs in `core-release`; the root GOWORK=off
# consumer build remains in `vet`.
#
# Each non-browser gate preserves CI fidelity; browser parity lives in its own
# dispatcher because its two suites publish separate evidence before one verdict.
# Per-gate CI-job parity is recorded in docs/.orchestration/m1/make-ci.md.

# Ordinary validation uses a reserved prerelease version that release-ref
# resolution rejects. `override` prevents a caller from converting this gate
# into release evidence through an environment or command-line assignment.
# The archive source is an explicit commit object; live index/worktree bytes are
# never prospective publication evidence.
# The POSIX entry point itself makes linux-ext4 mandatory on Linux so this thin
# dispatcher cannot accidentally select a skippable release sweep.
override CORE_ARTIFACT_VERSION := v0.0.0-ci
CORE_ARTIFACT_COMMIT_SHA ?= $(shell git rev-parse --verify HEAD)
GATES := vet core-release race vectors coverage network web browser hygiene lint sloc
# The composed CI graph has already established web dependencies before the
# browser gate. A distinct private target makes that authority explicit while
# keeping `make browser` independently self-sufficient.
CI_GATES := $(subst browser,browser-ci,$(GATES))
SCRIPT_GATES := $(filter-out core-release,$(GATES))
LOCAL_ENTRYPOINTS := check browser-contract browser-stability browser-network workflow-lint

ifeq ($(OS),Windows_NT)
DISPATCH = pwsh -NoProfile -File scripts/ci/$@.ps1
else
DISPATCH = bash scripts/ci/$@.sh
endif

# Gates share the worktree, the Go build cache and the exclusive D5 harness
# lease, so parallel `make -j` would only interleave failures.
.NOTPARALLEL:

.PHONY: ci browser-ci $(GATES) $(LOCAL_ENTRYPOINTS)

ci: $(CI_GATES)
	@echo "ci: all gates passed"

core-release:
ifeq ($(OS),Windows_NT)
	pwsh -NoProfile -File scripts/ci/core-release.ps1 -Version "$(CORE_ARTIFACT_VERSION)" -CommitSHA "$(CORE_ARTIFACT_COMMIT_SHA)"
else
	bash scripts/ci/core-release.sh "$(CORE_ARTIFACT_VERSION)" "$(CORE_ARTIFACT_COMMIT_SHA)"
endif

browser-ci:
ifeq ($(OS),Windows_NT)
	pwsh -NoProfile -File scripts/ci/browser.ps1 -SkipDependencyInstall
else
	bash scripts/ci/browser.sh --skip-dependency-install
endif

$(SCRIPT_GATES):
	$(DISPATCH)

$(LOCAL_ENTRYPOINTS):
	$(DISPATCH)
