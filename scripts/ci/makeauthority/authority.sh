#!/usr/bin/env bash

readonly WINDSHARE_GIT_PROBE_SHA1='162a4aaeeeb4392ac349fe67dc0178bc5ecaa60b'

windshare_enter_make_authority() {
  local candidate="${1:-}"
  local executable_header
  local probe
  local probe_token
  local version
  local version_output
  local name

  if [[ "${WINDSHARE_MAKE_AUTHORITY_ACTIVE:-}" == 1 ]]; then
    echo 'WindShare Make authority may be settled only once per process' >&2
    return 1
  fi
  for name in MAKEFLAGS MFLAGS GNUMAKEFLAGS MAKEFILES WINDSHARE_MAKE_AUTHORITY_ACTIVE WINDSHARE_MAKE_EXECUTABLE; do
    if [[ -v "$name" ]]; then
      echo "$name must be absent until WindShare Make authority is settled" >&2
      return 1
    fi
  done
  if [[ -z "$candidate" ]]; then
    candidate="$(type -P make || true)"
  fi
  if [[ "$candidate" != /* || ! -f "$candidate" || ! -x "$candidate" ]]; then
    echo 'WindShare Make authority requires one real executable application' >&2
    return 1
  fi
  exec {WINDSHARE_MAKE_DESCRIPTOR}<"$candidate"
  declare -gr WINDSHARE_MAKE_DESCRIPTOR
  # $$ remains the parent shell PID inside a Bash subshell; BASHPID names the
  # process that actually owns this descriptor.
  declare -gr WINDSHARE_MAKE_EXECUTABLE="/proc/$BASHPID/fd/$WINDSHARE_MAKE_DESCRIPTOR"
  if ! LC_ALL=C IFS= read -r -N 4 executable_header < "$WINDSHARE_MAKE_EXECUTABLE" ||
    [[ "$executable_header" != $'\x7fELF' ]]; then
    echo 'WindShare Make authority accepts only a retained native ELF application' >&2
    return 1
  fi

  version_output="$("$WINDSHARE_MAKE_EXECUTABLE" --version)" || return 1
  if [[ "${version_output%%$'\n'*}" =~ ^GNU\ Make\ ([0-9]+\.[0-9]+(\.[0-9]+)?)$ ]]; then
    version="${BASH_REMATCH[1]}"
  else
    echo 'WindShare Make application does not identify itself as GNU Make' >&2
    return 1
  fi
  IFS= read -r probe_token < /proc/sys/kernel/random/uuid || return 1
  probe_token="${probe_token//-/}"
  if [[ ! "$probe_token" =~ ^[0-9a-f]{32}$ ]]; then
    echo 'WindShare Make authority could not create its semantic challenge' >&2
    return 1
  fi
  probe="$(printf 'windshare_make_identity:\n\t@printf '\''%%s\\n'\'' '\''windshare-make-identity:%s:$(MAKE_VERSION)'\''\n' "$probe_token" \
    | "$WINDSHARE_MAKE_EXECUTABLE" -Rr --no-print-directory -f - windshare_make_identity)" || return 1
  if [[ "$probe" != "windshare-make-identity:$probe_token:$version" ]]; then
    echo 'WindShare Make application failed the controlled GNU Make semantic probe' >&2
    return 1
  fi

  declare -gr WINDSHARE_MAKE_CANDIDATE="$candidate"
  declare -gr WINDSHARE_MAKE_VERSION="$version"
  declare -gr WINDSHARE_MAKE_AUTHORITY_ACTIVE=1
}

windshare_make() {
  if [[ "${WINDSHARE_MAKE_AUTHORITY_ACTIVE:-}" != 1 || ! -r "${WINDSHARE_MAKE_EXECUTABLE:-/nonexistent}" ]]; then
    echo 'WindShare Make authority was not retained before use' >&2
    return 1
  fi
  "$WINDSHARE_MAKE_EXECUTABLE" "$@"
}

windshare_enter_makefile_authority() {
  local candidate="${1:-}"
  local expected_sha256="${2:-}"

  if [[ -v WINDSHARE_MAKEFILE_DESCRIPTOR || -v WINDSHARE_RETAINED_MAKEFILE ]]; then
    echo 'WindShare Makefile authority may be settled only once per process' >&2
    return 1
  fi
  if [[ "$candidate" != /* || ! -f "$candidate" || ! -r "$candidate" ]]; then
    echo 'WindShare Makefile authority requires one readable absolute regular file' >&2
    return 1
  fi
  if [[ ! "$expected_sha256" =~ ^[0-9a-f]{64}$ ]]; then
    echo 'WindShare Makefile authority requires the entry snapshot SHA-256' >&2
    return 1
  fi
  exec {WINDSHARE_MAKEFILE_DESCRIPTOR}<"$candidate"
  declare -gr WINDSHARE_MAKEFILE_DESCRIPTOR
  declare -gr WINDSHARE_RETAINED_MAKEFILE="/proc/$BASHPID/fd/$WINDSHARE_MAKEFILE_DESCRIPTOR"
  # The entry process exposes an unlinked snapshot descriptor. Reopening that
  # descriptor here transfers the same immutable parser object into this owner.
  declare -gr WINDSHARE_MAKEFILE_SHA256="$expected_sha256"
}

windshare_enter_git_authority() {
  local candidate="${1:-}"
  local executable_header
  local probe
  local version_output

  if [[ -v WINDSHARE_GIT_DESCRIPTOR || -v WINDSHARE_GIT_EXECUTABLE ]]; then
    echo 'WindShare Git authority may be settled only once per process' >&2
    return 1
  fi
  if [[ -z "$candidate" ]]; then
    candidate="$(type -P git || true)"
  fi
  if [[ "$candidate" != /* || ! -f "$candidate" || ! -x "$candidate" ]]; then
    echo 'WindShare Git authority requires one real executable application' >&2
    return 1
  fi
  exec {WINDSHARE_GIT_DESCRIPTOR}<"$candidate"
  declare -gr WINDSHARE_GIT_DESCRIPTOR
  declare -gr WINDSHARE_GIT_EXECUTABLE="/proc/$BASHPID/fd/$WINDSHARE_GIT_DESCRIPTOR"
  if ! LC_ALL=C IFS= read -r -N 4 executable_header < "$WINDSHARE_GIT_EXECUTABLE" ||
    [[ "$executable_header" != $'\x7fELF' ]]; then
    echo 'WindShare Git authority accepts only a retained native ELF application' >&2
    return 1
  fi
  version_output="$("$WINDSHARE_GIT_EXECUTABLE" --version)" || return 1
  if [[ ! "$version_output" =~ ^git\ version\ [0-9]+\.[0-9]+(\.[0-9]+)?([^[:space:]]*)?$ ]]; then
    echo 'WindShare Git application does not identify itself as Git' >&2
    return 1
  fi
  probe="$(printf '%s\n' 'windshare-git-authority-v1' | "$WINDSHARE_GIT_EXECUTABLE" hash-object --stdin)" || return 1
  if [[ "$probe" != "$WINDSHARE_GIT_PROBE_SHA1" ]]; then
    echo 'WindShare Git application failed the controlled object semantic probe' >&2
    return 1
  fi
  declare -gr WINDSHARE_GIT_CANDIDATE="$candidate"
}

windshare_git_head_commit() {
  local repository_root="${1:-}"
  local commit

  if [[ ! -x "${WINDSHARE_GIT_EXECUTABLE:-/nonexistent}" || "$repository_root" != /* ]]; then
    echo 'WindShare Git authority was not retained before checkout inspection' >&2
    return 1
  fi
  commit="$(GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
    GIT_CONFIG_COUNT=0 GIT_ATTR_NOSYSTEM=1 "$WINDSHARE_GIT_EXECUTABLE" --no-replace-objects \
    -C "$repository_root" rev-parse --verify 'HEAD^{commit}')" || return 1
  if [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
    echo 'WindShare Git authority did not resolve one SHA-1 commit object' >&2
    return 1
  fi
  printf '%s\n' "$commit"
}
