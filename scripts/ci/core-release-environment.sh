#!/usr/bin/env bash

# Release evidence must not depend on a caller's Go configuration or cache
# history. The owned release root gives every invocation a new, disposable
# trust boundary without weakening the platform-specific temp-root checks.
windshare_prepare_core_release_go_environment() {
  local release_root="$1"
  local resolved_root
  local module_cache_path
  local build_cache_path
  local go_path
  local cache_path

  if [ -z "$release_root" ] || [ ! -d "$release_root" ] || [ -L "$release_root" ]; then
    echo "core release environment requires an existing no-follow release root" >&2
    return 1
  fi
  resolved_root="$(cd -- "$release_root" && pwd -P)" || return 1

  module_cache_path="$resolved_root/go-module-cache"
  build_cache_path="$resolved_root/go-build-cache"
  go_path="$resolved_root/go-path"
  for cache_path in "$module_cache_path" "$build_cache_path" "$go_path"; do
    if [ -e "$cache_path" ] || [ -L "$cache_path" ]; then
      echo "core release cache path must be fresh: $cache_path" >&2
      return 1
    fi
  done
  for cache_path in "$module_cache_path" "$build_cache_path" "$go_path"; do
    install -d -m 0700 -- "$cache_path" || return 1
  done

  # Bash preserves an existing variable's export attribute across assignment.
  # Publish paths only after every fallible setup step so a rejected release root
  # cannot replace either exported caller values or initially unset shell state.
  GOMODCACHE="$module_cache_path"
  GOCACHE="$build_cache_path"
  GOPATH="$go_path"

  # Explicit target knobs override the host defaults even with GOENV disabled.
  # Clearing them prevents a caller from silently turning native certification
  # into a cross-build or changing the compiler experiment set.
  unset GOOS GOARCH CGO_ENABLED GOEXPERIMENT
  GOENV=off
  # Go normally makes downloaded module directories read-only. That protects a
  # shared long-lived cache, but this cache is private and disposable; retaining
  # owner write permission is what lets the owning release process remove it.
  GOFLAGS=-modcacherw
  GOTOOLCHAIN=local
  GOWORK=off
  GOPROXY=https://proxy.golang.org
  GOSUMDB=sum.golang.org
  GOPRIVATE=''
  GONOSUMDB=''
  GONOPROXY=''
  GOINSECURE=''
  GOTELEMETRY=off
  export GOMODCACHE GOCACHE GOPATH GOENV GOFLAGS GOTOOLCHAIN GOWORK
  export GOPROXY GOSUMDB GOPRIVATE GONOSUMDB GONOPROXY GOINSECURE GOTELEMETRY
}
