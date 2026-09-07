# The root Makefile intentionally serializes every invocation. This isolated
# scheduler supplies bounded concurrency while each recursive lane retains the
# root ordering and platform dispatch contracts.
REPOSITORY_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
include scripts/ci/ci-gates.mk

.PHONY: ci-parallel $(CI_PARALLEL_LANES)

ci-parallel: $(CI_PARALLEL_LANES)
	@echo "ci-parallel: all production source gates passed"

# Long-running gates report their own milestones; buffering an entire recipe
# hides useful progress until after the gate has finished.
$(CI_PARALLEL_LANES):
	@echo "== $@: START =="
	+@$(MAKE) --no-print-directory --output-sync=none -C "$(REPOSITORY_ROOT)" $@
