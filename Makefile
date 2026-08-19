# Local gates consume the developer's installed toolchain and the committed
# production module graph, so validation cannot mutate tool versions or inherit
# an ambient workspace.
override GOTOOLCHAIN := local
override GOWORK := off
export GOTOOLCHAIN GOWORK

PUBLIC_TARGETS := ci ci-parallel ci-full check hygiene sloc workflow-lint lint vet short-go race coverage vectors vectors-update web e2e browser browser-weekly gopls long-go
include scripts/ci/ci-gates.mk

INTERNAL_TARGETS := browser-weekly-supplement $(CI_PARALLEL_LANES)
COMPOSITE_TARGETS := ci ci-parallel ci-full browser-weekly $(CI_PARALLEL_LANES)
PLATFORM_TARGETS := $(filter-out $(COMPOSITE_TARGETS),$(PUBLIC_TARGETS) $(INTERNAL_TARGETS))

ifeq ($(OS),Windows_NT)
RUN_PLATFORM_GATE = pwsh -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/$@.ps1
else
RUN_PLATFORM_GATE = bash scripts/ci/linux/$@.sh
endif

# Gate order is part of the default local feedback contract. The explicit
# ci-parallel target delegates only the three reviewed resource-domain lanes to
# a separate make instance; arbitrary `make -j` combinations remain serialized.
.NOTPARALLEL:
.DEFAULT_GOAL := ci
.PHONY: $(PUBLIC_TARGETS) $(INTERNAL_TARGETS)

ci: $(CI_GATES)
	@echo "ci: all production source gates passed"

ci-parallel:
	+@$(MAKE) --no-print-directory -j$(words $(CI_PARALLEL_LANES)) -f scripts/ci/ci-parallel.mk ci-parallel

ci-parallel-runtime: $(CI_PARALLEL_RUNTIME_GATES)
	@echo "ci-parallel: runtime lane passed"

ci-parallel-web: $(CI_PARALLEL_WEB_GATES)
	@echo "ci-parallel: web lane passed"

ci-parallel-static: $(CI_PARALLEL_STATIC_GATES)
	@echo "ci-parallel: static lane passed"

# `browser` runs `test:browser:smoke` followed by `test:browser:contract:short`.
# The ordinary CI therefore owns both Chromium short lanes; reusing only the
# weekly supplement keeps the full current-host sweep complete without rerunning
# either ordinary Chromium project.
ci-full: ci long-go browser-weekly-supplement
	@echo "ci-full: all current-host source gates passed"

browser-weekly: browser browser-weekly-supplement
	@echo "browser-weekly: all gates passed"

$(PLATFORM_TARGETS):
	$(RUN_PLATFORM_GATE)
