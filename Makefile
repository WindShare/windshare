# Local CI mirror. Platform leaves dispatch to matching scripts; the few public
# composites and parameterized authority boundaries are declared explicitly here.
#
# Usage: `make ci` mirrors the current OS's pull-request gates. Browser validation
# joins the unprivileged `browser-local` leaf with a token-free, content-bound
# completion consumer. The workflow-only OIDC broker publishes that completion
# before the public graph can execute it. Every ordinary gate remains independently
# invokable through Make (for example, `make race`). Windows runtime:
# budget ~10 minutes warm and
# ~30 minutes cold, including the extracted core release sweep.
# The core artifact invariant runs in `core-release`; the root GOWORK=off
# consumer build remains in `vet`.
#
# Each non-browser gate preserves CI fidelity; full-browser parity lives in its
# own dispatcher because its two suites publish separate evidence before one verdict.

# Public Make owns its graph and accepts only target names. The retained launcher
# activates a separate mode for CI inputs whose exact file and interpreter
# identities have already been locked. GNU Make does not expose a dedicated
# "effective dry-run" bit, so both modes reject parser, shell, and control flags
# before a recipe can dispatch.
ifneq ($(origin SHELL),default)
$(error SHELL is Makefile-owned and cannot be supplied)
endif
ifneq ($(origin .SHELLFLAGS),default)
$(error .SHELLFLAGS is Makefile-owned and cannot be supplied)
endif
ifneq ($(origin VALIDATION_COMMAND_LINE_VARIABLES),undefined)
$(error VALIDATION_COMMAND_LINE_VARIABLES is Makefile-owned and cannot be supplied)
endif
ifneq ($(origin VALIDATION_RETAINED_MODE),undefined)
$(error VALIDATION_RETAINED_MODE is Makefile-owned and cannot be supplied)
endif
override VALIDATION_RETAINED_MODE := $(if $(filter command line,$(origin WINDSHARE_RETAINED_MAKEFILE)),1,0)
override VALIDATION_COMMAND_LINE_VARIABLES := $(strip $(foreach variable,$(.VARIABLES),$(if $(filter command line,$(origin $(variable))),$(variable))))
ifeq ($(VALIDATION_RETAINED_MODE),1)
ifneq ($(filter-out BROWSER_NETWORK_COMPLETION WINDSHARE_HOST_GOOS WINDSHARE_CORE_ARTIFACT_COMMIT_SHA WINDSHARE_RETAINED_MAKEFILE WINDSHARE_RECIPE_SHELL WINDSHARE_BASH_EXECUTABLE WINDSHARE_PWSH_EXECUTABLE,$(VALIDATION_COMMAND_LINE_VARIABLES)),)
$(error validation accepts only caller path operands and retained-launcher variables)
endif
else
ifneq ($(VALIDATION_COMMAND_LINE_VARIABLES),)
$(error public validation accepts target names, not command-line variable assignments)
endif
endif
ifneq ($(filter environment environment override,$(origin MAKEFLAGS)),)
$(error MAKEFLAGS may not be supplied through the environment)
endif
ifneq ($(filter-out --,$(firstword $(MAKEFLAGS))),)
$(error GNU Make execution flags are forbidden for validation targets)
endif
ifneq ($(strip $(MFLAGS)),)
$(error GNU Make control flags are forbidden for validation targets)
endif
ifneq ($(strip $(GNUMAKEFLAGS)),)
$(error GNUMAKEFLAGS is forbidden for validation targets)
endif
ifneq ($(strip $(MAKEFILES)),)
$(error MAKEFILES may not inject validation makefiles)
endif
ifneq ($(strip $(GOFLAGS)),)
$(error GOFLAGS may not alter validation test selection)
endif
ifneq ($(strip $(GOWORK)),)
$(error GOWORK is owned by each validation leaf and cannot be supplied)
endif
ifneq ($(strip $(GOOS)),)
$(error GOOS cannot replace the native validation platform)
endif
ifneq ($(strip $(GOARCH)),)
$(error GOARCH cannot replace the native validation architecture)
endif
ifneq ($(strip $(GOENV)),)
$(error GOENV cannot replace validation toolchain configuration)
endif
ifneq ($(strip $(GOTOOLCHAIN)),)
$(error GOTOOLCHAIN cannot replace the validated Go toolchain)
endif
ifneq ($(strip $(GOROOT)),)
$(error GOROOT cannot replace the validated Go installation)
endif
ifeq ($(origin OS),command line)
$(error OS is not a caller-owned platform authority)
endif
ifeq ($(origin COMSPEC),command line)
$(error COMSPEC is not a caller-owned shell authority)
endif
ifneq ($(origin HOST_GOOS),undefined)
$(error HOST_GOOS is Makefile-owned and cannot be supplied)
endif
ifeq ($(VALIDATION_RETAINED_MODE),1)
ifneq ($(origin WINDSHARE_HOST_GOOS),command line)
$(error validation must receive its host platform from the retained Make launcher)
endif
ifneq ($(filter $(WINDSHARE_HOST_GOOS),linux windows),$(WINDSHARE_HOST_GOOS))
$(error retained Make launcher supplied an unsupported host platform)
endif
override HOST_GOOS := $(WINDSHARE_HOST_GOOS)
ifeq ($(filter $(HOST_GOOS),linux windows),)
$(error unsupported host GOOS $(HOST_GOOS))
endif

ifneq ($(origin WINDSHARE_RETAINED_MAKEFILE),command line)
$(error validation must receive its parser input from the retained Make launcher)
endif
ifeq ($(strip $(WINDSHARE_RETAINED_MAKEFILE)),)
$(error retained Make launcher supplied an empty parser authority)
endif
ifneq ($(origin WINDSHARE_RECIPE_SHELL),command line)
$(error validation must receive its recipe shell from the retained Make launcher)
endif
ifeq ($(strip $(WINDSHARE_RECIPE_SHELL)),)
$(error retained Make launcher supplied an empty recipe shell authority)
endif
override SHELL := $(WINDSHARE_RECIPE_SHELL)
override .SHELLFLAGS := -eu -c
ifeq ($(HOST_GOOS),windows)
ifneq ($(origin WINDSHARE_PWSH_EXECUTABLE),command line)
$(error Windows validation must receive its pwsh interpreter from the retained Make launcher)
endif
ifeq ($(strip $(WINDSHARE_PWSH_EXECUTABLE)),)
$(error retained Make launcher supplied an empty pwsh authority)
endif
else
ifneq ($(origin WINDSHARE_BASH_EXECUTABLE),command line)
$(error Linux validation must receive its Bash interpreter from the retained Make launcher)
endif
ifeq ($(strip $(WINDSHARE_BASH_EXECUTABLE)),)
$(error retained Make launcher supplied an empty Bash authority)
endif
endif
else
$(foreach variable,BROWSER_NETWORK_COMPLETION WINDSHARE_HOST_GOOS WINDSHARE_CORE_ARTIFACT_COMMIT_SHA WINDSHARE_RETAINED_MAKEFILE WINDSHARE_RECIPE_SHELL WINDSHARE_BASH_EXECUTABLE WINDSHARE_PWSH_EXECUTABLE,$(if $(filter-out undefined,$(origin $(variable))),$(error $(variable) is reserved for the retained Make launcher)))
ifeq ($(OS),Windows_NT)
override HOST_GOOS := windows
override WINDSHARE_PWSH_EXECUTABLE := pwsh
else
override HOST_GOOS := linux
override WINDSHARE_BASH_EXECUTABLE := /bin/bash
endif
endif

# Ordinary validation uses a reserved prerelease version that release-ref
# resolution rejects. `override` prevents a caller from converting this gate
# into release evidence through an environment or command-line assignment.
# The archive source is an explicit commit object; live index/worktree bytes are
# never prospective publication evidence.
# The POSIX entry point itself makes linux-ext4 mandatory on Linux so this thin
# dispatcher cannot accidentally select a skippable release sweep.
ifneq ($(origin CORE_ARTIFACT_VERSION),undefined)
$(error CORE_ARTIFACT_VERSION is Makefile-owned and cannot be supplied)
endif
override CORE_ARTIFACT_VERSION := v0.0.0-ci
ifeq ($(VALIDATION_RETAINED_MODE),1)
ifneq ($(origin WINDSHARE_CORE_ARTIFACT_COMMIT_SHA),command line)
$(error validation must receive checkout identity from the retained Git launcher)
endif
else
override WINDSHARE_CORE_ARTIFACT_COMMIT_SHA := $(strip $(shell git rev-parse --verify HEAD))
endif
override CORE_ARTIFACT_COMMIT_SHA := $(WINDSHARE_CORE_ARTIFACT_COMMIT_SHA)
ifeq ($(strip $(CORE_ARTIFACT_COMMIT_SHA)),)
$(error CORE_ARTIFACT_COMMIT_SHA could not be derived from the current checkout)
endif
export WINDSHARE_CORE_ARTIFACT_COMMIT_SHA
export BROWSER_NETWORK_COMPLETION
ifneq ($(origin PLATFORM_ENTRYPOINTS),undefined)
$(error PLATFORM_ENTRYPOINTS is Makefile-owned and cannot be supplied)
endif
override PLATFORM_ENTRYPOINTS := browser-contract browser-generated browser-local browser-network browser-process browser-stability check core-release coverage e2e-go hygiene integration lint race sloc vectors vet web web-dependencies workflow-lint
ifneq ($(origin CI_GATES),undefined)
$(error CI_GATES is Makefile-owned and cannot be supplied)
endif
override CI_GATES := vet core-release race vectors coverage integration e2e web browser-contract browser-generated browser-process hygiene lint sloc
# Full validation is the public pull-request graph plus the full Browsergate
# composite. The composite consumes only a completion artifact; minted identity
# bytes never enter Make or any of its descendants.
ifneq ($(origin CI_FULL_GATES),undefined)
$(error CI_FULL_GATES is Makefile-owned and cannot be supplied)
endif
override CI_FULL_GATES := vet core-release race vectors coverage integration e2e web browser-contract browser-generated browser-process hygiene lint sloc browser
ifneq ($(origin DISPATCH_ENTRYPOINTS),undefined)
$(error DISPATCH_ENTRYPOINTS is Makefile-owned and cannot be supplied)
endif
override DISPATCH_ENTRYPOINTS := $(filter-out browser-network core-release web-dependencies,$(PLATFORM_ENTRYPOINTS))

ifneq ($(origin DISPATCH),undefined)
$(error DISPATCH is Makefile-owned and cannot be supplied)
endif
ifeq ($(HOST_GOOS),windows)
override DISPATCH = "$(WINDSHARE_PWSH_EXECUTABLE)" -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/$@.ps1
else
override DISPATCH = "$(WINDSHARE_BASH_EXECUTABLE)" scripts/ci/linux/$@.sh
endif

# Gates share the worktree, Go build cache, and browser evidence directories, so
# parallel `make -j` would make failures harder to attribute to one owner.
.NOTPARALLEL:

.PHONY: authority-context ci ci-full e2e browser browser-smoke plan-ci plan-ci-full plan-browser $(PLATFORM_ENTRYPOINTS)

# This deferred guard sees the complete parser input, including makefiles added
# with -f after the canonical Makefile. It therefore closes a gap that cannot be
# decided while the first makefile is still being read.
authority-context:
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error validation requires exactly one Makefile))
ifeq ($(VALIDATION_RETAINED_MODE),1)
	$(if $(and $(filter $(WINDSHARE_RETAINED_MAKEFILE),$(firstword $(MAKEFILE_LIST))),$(filter $(firstword $(MAKEFILE_LIST)),$(WINDSHARE_RETAINED_MAKEFILE))),,$(error validation requires the retained Makefile identity))
else
	$(if $(filter $(realpath Makefile),$(realpath $(firstword $(MAKEFILE_LIST)))),,$(error public validation requires the repository Makefile identity))
endif
	@:

ci: authority-context $(CI_GATES)
	@echo "ci: all gates passed"

ci-full: authority-context $(CI_FULL_GATES)
	@echo "ci-full: all gates passed"

browser: authority-context browser-local browser-network
	@echo "browser: local and network gates passed"

browser-network: authority-context
ifeq ($(HOST_GOOS),windows)
	"$(WINDSHARE_PWSH_EXECUTABLE)" -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/browser-network.ps1
else
	"$(WINDSHARE_BASH_EXECUTABLE)" scripts/ci/linux/browser/network-completion.sh
endif

# Plan targets expose graph membership without GNU Make's execution-suppressing
# flags, which are unsafe evidence for whether a validation gate really ran.
plan-ci: authority-context
	@printf '%s\n' $(addprefix gate:,$(CI_GATES))

plan-ci-full: authority-context
	@printf '%s\n' $(addprefix gate:,$(CI_FULL_GATES))

plan-browser: authority-context
	@printf '%s\n' gate:browser-local gate:browser-network

# Every Node consumer must also work when invoked from a clean checkout. Make
# deduplicates this shared prerequisite within one invocation while hosted jobs
# remain free to execute the same platform entrypoint in isolated workspaces.
check web browser-contract browser-generated browser-local browser-process browser-stability: authority-context web-dependencies

# The public E2E gate preserves platform semantics without making Linux execute
# a fake smoke leaf. Full validation retains this PR-equivalent tuple before it
# appends the broader Browsergate authority.
ifeq ($(HOST_GOOS),windows)
e2e: authority-context e2e-go browser-smoke

browser-smoke: authority-context web-dependencies
	"$(WINDSHARE_PWSH_EXECUTABLE)" -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/browser/smoke.ps1
else
e2e: authority-context e2e-go
endif

core-release: authority-context
ifeq ($(HOST_GOOS),windows)
	"$(WINDSHARE_PWSH_EXECUTABLE)" -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/core-release.ps1 -Version "$(CORE_ARTIFACT_VERSION)" -CommitSHA "$(CORE_ARTIFACT_COMMIT_SHA)"
else
	"$(WINDSHARE_BASH_EXECUTABLE)" scripts/ci/linux/core-release.sh "$(CORE_ARTIFACT_VERSION)" "$(CORE_ARTIFACT_COMMIT_SHA)"
endif

# Dependency acquisition is a literal leaf so every higher-level gate shares one
# exact prerequisite without being able to hide another validation tuple below it.
web-dependencies: authority-context
	$(DISPATCH)

$(DISPATCH_ENTRYPOINTS): authority-context
	$(DISPATCH)
