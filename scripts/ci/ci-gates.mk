# Runtime and protocol failures carry the highest product risk, so the serial
# entry point surfaces them before slower-to-act-on static diagnostics.
CI_GATES := short-go vectors web e2e browser hygiene workflow-lint lint vet gopls sloc

# Parallel lanes separate the principal local resource domains. Web build and
# browser work intentionally share a lane because both own the same Web tree.
CI_PARALLEL_RUNTIME_GATES := short-go vectors e2e
CI_PARALLEL_WEB_GATES := web browser
CI_PARALLEL_STATIC_GATES := hygiene workflow-lint lint vet gopls sloc
CI_PARALLEL_GATES := $(CI_PARALLEL_RUNTIME_GATES) $(CI_PARALLEL_WEB_GATES) $(CI_PARALLEL_STATIC_GATES)
CI_PARALLEL_LANES := ci-parallel-runtime ci-parallel-web ci-parallel-static

# Every ordinary gate has one parallel owner. Failing while parsing is preferable
# to silently weakening ci-parallel when the ordinary gate set changes.
ifneq ($(words $(CI_GATES)),$(words $(sort $(CI_GATES))))
$(error CI_GATES contains duplicate targets)
endif
ifneq ($(words $(CI_PARALLEL_GATES)),$(words $(sort $(CI_PARALLEL_GATES))))
$(error an ordinary gate belongs to more than one ci-parallel lane)
endif
ifneq ($(sort $(CI_GATES)),$(sort $(CI_PARALLEL_GATES)))
$(error ci and ci-parallel must own the same gate set)
endif
