# Local gates are developer conveniences, not an authority boundary. Make selects the
# native script by gate name; each script owns its toolchain invocation and evidence.
#
# `make ci` composes the ordinary local graph. Race and coverage already exercise the
# integration and Go E2E packages, so their focused leaves are intentionally absent.

PUBLIC_TARGETS := ci check hygiene sloc lint workflow-lint vet race coverage vectors core-release web web-dependencies integration e2e-go e2e browser-preflight browser-process browser-smoke browser-local browser-network browser-stability browser
PLATFORM_ENTRYPOINTS := browser-local browser-network browser-preflight browser-process browser-stability check core-release coverage e2e-go hygiene integration lint race sloc vectors vet web web-dependencies workflow-lint

# Keep cheap failures ahead of expensive evidence when the local graph runs serially.
CI_GATES := hygiene sloc workflow-lint lint vet race coverage vectors core-release web browser-preflight browser-process
WINDOWS_CI_GATES := browser-smoke
WINDOWS_GATE_SELECTION = $(if $(filter Windows_NT,$(OS)),$(WINDOWS_CI_GATES))
LOCAL_CI_GATES = $(CI_GATES) $(WINDOWS_GATE_SELECTION)
LOCAL_E2E_GATES = e2e-go $(WINDOWS_GATE_SELECTION)

ifeq ($(OS),Windows_NT)
RUN_PLATFORM_GATE = pwsh -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/$@.ps1
else
RUN_PLATFORM_GATE = bash scripts/ci/linux/$@.sh
endif

DISPATCH_ENTRYPOINTS := $(filter-out core-release,$(PLATFORM_ENTRYPOINTS))
NODE_GATES := check web browser-preflight browser-local browser-process browser-stability
override CORE_RELEASE_VERSION := v0.0.0-ci
CORE_RELEASE_COMMIT = $(shell git rev-parse --verify HEAD)

# Gates share caches and evidence directories, so one local graph stays serial even
# when a caller enables GNU Make job parallelism.
.NOTPARALLEL:

.PHONY: $(filter-out browser-smoke,$(PUBLIC_TARGETS))

ci: $(LOCAL_CI_GATES)
	@echo "ci: all gates passed"

e2e: $(LOCAL_E2E_GATES)

browser: browser-local browser-network
	@echo "browser: local and network gates passed"

$(NODE_GATES): web-dependencies

core-release:
ifeq ($(OS),Windows_NT)
	pwsh -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/core-release.ps1 -Version "$(CORE_RELEASE_VERSION)" -CommitSHA "$(CORE_RELEASE_COMMIT)"
else
	bash scripts/ci/linux/core-release.sh "$(CORE_RELEASE_VERSION)" "$(CORE_RELEASE_COMMIT)"
endif

ifeq ($(OS),Windows_NT)
.PHONY: browser-smoke
browser-smoke: web-dependencies
	pwsh -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/browser/smoke.ps1
endif

$(DISPATCH_ENTRYPOINTS):
	$(RUN_PLATFORM_GATE)
