#!/usr/bin/env bash

# Release evidence must not depend on a caller's Go configuration or cache
# history. The owned release root gives every invocation a new, disposable
# trust boundary without weakening the platform-specific temp-root checks.
windshare_prepare_core_release_go_environment() {
  local release_root="$1"
  local resolved_root
  local cache_path

  if [ -z "$release_root" ] || [ ! -d "$release_root" ] || [ -L "$release_root" ]; then
    echo "core release environment requires an existing no-follow release root" >&2
    return 1
  fi
  resolved_root="$(cd -- "$release_root" && pwd -P)" || return 1

  GOMODCACHE="$resolved_root/go-module-cache"
  GOCACHE="$resolved_root/go-build-cache"
  GOPATH="$resolved_root/go-path"
  for cache_path in "$GOMODCACHE" "$GOCACHE" "$GOPATH"; do
    if [ -e "$cache_path" ] || [ -L "$cache_path" ]; then
      echo "core release cache path must be fresh: $cache_path" >&2
      return 1
    fi
  done
  for cache_path in "$GOMODCACHE" "$GOCACHE" "$GOPATH"; do
    install -d -m 0700 -- "$cache_path" || return 1
  done

  # Explicit target knobs override the host defaults even with GOENV disabled.
  # Clearing them prevents a caller from silently turning native certification
  # into a cross-build or changing the compiler experiment set.
  unset GOOS GOARCH CGO_ENABLED GOEXPERIMENT
  GOENV=off
  GOFLAGS=''
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
