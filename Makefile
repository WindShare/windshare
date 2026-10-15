# Local gates consume the developer's installed toolchain and the committed
# production module graph, so validation cannot mutate tool versions or inherit
# an ambient workspace.
override GOTOOLCHAIN := local
override GOWORK := off
export GOTOOLCHAIN GOWORK

PUBLIC_TARGETS := ci ci-parallel ci-full check hygiene sloc workflow-lint lint vet short-go race coverage vectors vectors-update rust web e2e browser browser-weekly gopls long-go
include scripts/ci/ci-gates.mk

INTERNAL_TARGETS := browser-weekly-supplement $(CI_PARALLEL_LANES)
COMPOSITE_TARGETS := ci ci-parallel ci-full browser-weekly $(CI_PARALLEL_LANES)
PORTABLE_TARGETS := rust
PLATFORM_TARGETS := $(filter-out $(COMPOSITE_TARGETS) $(PORTABLE_TARGETS),$(PUBLIC_TARGETS) $(INTERNAL_TARGETS))

# rustup run fails when stable is missing instead of installing it during a gate.
# Bound Cargo workers because two other CI lanes can be active at the same time.
RUST_CARGO := rustup run stable cargo
RUST_WORKSPACE := relay-rust
RUST_BUILD_JOBS := 2
RUST_COVERAGE_MIN := 60
RUST_COVERAGE_PACKAGE_MIN := 50

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

check: rust

# Cargo owns its bounded workers; MSYS Make's jobserver handles cannot be used by
# native Windows Cargo. Keep scheduler state out of this leaf gate on both hosts.
rust: private export MAKEFLAGS :=
rust: private export MFLAGS :=
rust: private export CARGO_MAKEFLAGS :=
rust:
	@echo "== rust =="
	cd $(RUST_WORKSPACE) && $(RUST_CARGO) fmt --all -- --check
	cd $(RUST_WORKSPACE) && $(RUST_CARGO) clippy --workspace --all-targets --locked --jobs $(RUST_BUILD_JOBS) -- -D warnings
	node --test scripts/ci/rust/line-coverage.test.mjs
	node scripts/ci/rust/check-coverage.mjs "$(RUST_WORKSPACE)" $(RUST_COVERAGE_MIN) $(RUST_COVERAGE_PACKAGE_MIN) $(RUST_BUILD_JOBS)
	cd $(RUST_WORKSPACE) && $(RUST_CARGO) test --workspace --doc --locked --jobs $(RUST_BUILD_JOBS)
	cd $(RUST_WORKSPACE) && $(RUST_CARGO) build -p windshare-relay-core --target wasm32-unknown-unknown --locked --jobs $(RUST_BUILD_JOBS)
	@echo "== rust: PASS =="

$(PLATFORM_TARGETS):
	$(RUN_PLATFORM_GATE)
