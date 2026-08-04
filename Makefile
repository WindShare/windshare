# Local gates consume the developer's installed toolchain so validation never
# changes tool versions as a side effect of running a target.
override GOTOOLCHAIN := local
export GOTOOLCHAIN

PUBLIC_TARGETS := ci check hygiene sloc workflow-lint lint vet short-go race coverage vectors web e2e browser gopls long-go core-release
CI_GATES := hygiene sloc workflow-lint lint vet short-go vectors web e2e browser gopls
PLATFORM_TARGETS := $(filter-out ci core-release,$(PUBLIC_TARGETS))

override CORE_RELEASE_VERSION := v0.0.0-ci
CORE_RELEASE_COMMIT ?= $(shell git rev-parse --verify HEAD)

ifeq ($(OS),Windows_NT)
RUN_PLATFORM_GATE = pwsh -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/$@.ps1
else
RUN_PLATFORM_GATE = bash scripts/ci/linux/$@.sh
endif

# Gate order is part of the local feedback contract; allowing `make -j` to race
# shared compiler and browser state would make timings and failures surprising.
.NOTPARALLEL:
.DEFAULT_GOAL := ci
.PHONY: $(PUBLIC_TARGETS)

ci: $(CI_GATES)
	@echo "ci: all gates passed"

core-release:
ifeq ($(OS),Windows_NT)
	pwsh -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/core-release.ps1 -Version "$(CORE_RELEASE_VERSION)" -CommitSHA "$(CORE_RELEASE_COMMIT)"
else
	bash scripts/ci/linux/core-release.sh "$(CORE_RELEASE_VERSION)" "$(CORE_RELEASE_COMMIT)"
endif

$(PLATFORM_TARGETS):
	$(RUN_PLATFORM_GATE)
