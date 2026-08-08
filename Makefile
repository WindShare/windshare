# Local gates consume the developer's installed toolchain so validation never
# changes tool versions as a side effect of running a target.
override GOTOOLCHAIN := local
export GOTOOLCHAIN

PUBLIC_TARGETS := ci ci-full check hygiene sloc workflow-lint lint vet root-release-graph short-go race coverage vectors vectors-update web e2e browser browser-weekly gopls long-go core-release
INTERNAL_TARGETS := browser-weekly-supplement
# Runtime and protocol failures carry the highest product risk, so surface them
# before slower-to-act-on static diagnostics during iterative agent work.
CI_GATES := short-go vectors web e2e browser hygiene workflow-lint lint vet gopls sloc
PLATFORM_TARGETS := $(filter-out ci ci-full browser-weekly core-release,$(PUBLIC_TARGETS)) $(INTERNAL_TARGETS)

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
.PHONY: $(PUBLIC_TARGETS) $(INTERNAL_TARGETS)

ci: $(CI_GATES)
	@echo "ci: all workspace source gates passed"

# `browser` runs `test:browser:smoke` followed by `test:browser:contract:short`.
# The ordinary CI therefore owns both Chromium short lanes; reusing only the
# weekly supplement keeps the full current-host sweep complete without rerunning
# either ordinary Chromium project.
ci-full: ci long-go browser-weekly-supplement
	@echo "ci-full: all current-host source gates passed"

browser-weekly: browser browser-weekly-supplement
	@echo "browser-weekly: all gates passed"

core-release:
ifeq ($(OS),Windows_NT)
	pwsh -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/core-release.ps1 -Version "$(CORE_RELEASE_VERSION)" -CommitSHA "$(CORE_RELEASE_COMMIT)"
else
	bash scripts/ci/linux/core-release.sh "$(CORE_RELEASE_VERSION)" "$(CORE_RELEASE_COMMIT)"
endif

$(PLATFORM_TARGETS):
	$(RUN_PLATFORM_GATE)
