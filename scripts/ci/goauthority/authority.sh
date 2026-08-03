#!/usr/bin/env bash

# Validation must settle Go before repository code can influence toolchain or
# platform selection. The open descriptor makes the selected application the
# authority for the lifetime of the entrypoint, even if its PATH entry changes.
readonly WINDSHARE_STABILITY_GO_AUTHORITY_SEMANTICS='{"schema_version":"windshare.stability-helper-semantics/v1","operating_system":"linux","role":"go-executable-authority","revision":1,"command_plan":["reject-ambient-and-persisted-selection","retain-native-cmd-go","invoke-retained-go-with-owned-environment"]}'

# Hosted runners export GOTOOLCHAIN=local through actions/setup-go for every
# step, and the retained Go is always invoked with that same value. Ambient
# local is therefore the owned default rather than caller selection; any other
# GOTOOLCHAIN value remains rejected alongside every other selection variable.
windshare_go_selection_ambient() {
  [[ "$1" != GOTOOLCHAIN ]] || [[ "${!1}" != local ]]
}

# A settled entrypoint exports the owned bindings, so a subprocess that
# re-enters the authority sees them as ordinary environment variables, while an
# in-process re-entry finds the readonly bindings of this shell. Only the
# former may re-settle: the retained descriptor is process-private, so every
# entrypoint must re-validate its own Go application from PATH.
windshare_go_authority_inherited() {
  [[ "${WINDSHARE_GO_AUTHORITY_ACTIVE:-}" == 1 ]] &&
    ( WINDSHARE_GO_AUTHORITY_ACTIVE=1 ) 2>/dev/null
}

windshare_enter_go_authority() {
  local candidate
  local candidate_identity
  local changed_name
  local config_directory
  local config_path
  local executable_bytes
  local expected_executable
  local go_metadata
  local -a go_metadata_lines
  local go_root
  local host_arch
  local host_os
  local name
  local retained_executable

  if [[ "${WINDSHARE_GO_AUTHORITY_ACTIVE:-}" == 1 ]] &&
    ! windshare_go_authority_inherited; then
    echo 'WindShare Go authority may be settled only once per entrypoint' >&2
    return 1
  fi

  for name in GOFLAGS GOWORK GOOS GOARCH GOENV GOTOOLCHAIN GOROOT \
    WINDSHARE_GO_EXECUTABLE WINDSHARE_GO_AUTHORITY_ACTIVE \
    WINDSHARE_GO_HOST_OS WINDSHARE_GO_HOST_ARCH; do
    if windshare_go_authority_inherited && [[ "$name" == WINDSHARE_GO_* ]]; then
      # Owned exports of an ancestor authority, replaced by this re-settlement
      # before any consumer runs; only truly ambient selection must fail.
      continue
    fi
    if [[ -v "$name" ]] && windshare_go_selection_ambient "$name"; then
      echo "$name must be absent until WindShare Go authority is settled" >&2
      return 1
    fi
  done

  if [[ -n "${XDG_CONFIG_HOME:-}" ]]; then
    config_directory="$XDG_CONFIG_HOME"
  elif [[ -n "${HOME:-}" ]]; then
    config_directory="$HOME/.config"
  else
    echo 'WindShare Go authority cannot locate the persisted Go environment' >&2
    return 1
  fi
  config_path="$config_directory/go/env"
  if [[ -f "$config_path" ]]; then
    while IFS='=' read -r changed_name changed_value; do
      changed_name="${changed_name//$'\r'/}"
      changed_value="${changed_value//$'\r'/}"
      case "$changed_name" in
        GOFLAGS|GOWORK|GOOS|GOARCH|GOENV|GOROOT)
          echo "$changed_name must not be persisted outside WindShare Go authority" >&2
          return 1
          ;;
        GOTOOLCHAIN)
          if [[ "$changed_value" != local ]]; then
            echo 'GOTOOLCHAIN must not be persisted outside WindShare Go authority (owned default: local)' >&2
            return 1
          fi
          ;;
      esac
    done <"$config_path"
  fi

  candidate="$(type -P go || true)"
  if [[ -z "$candidate" || "$candidate" != /* || ! -f "$candidate" || ! -x "$candidate" ]]; then
    echo 'WindShare Go authority requires one real executable Go application' >&2
    return 1
  fi
  candidate="$(readlink -f -- "$candidate")"
  if [[ -z "$candidate" || ! -f "$candidate" || ! -x "$candidate" ]]; then
    echo 'WindShare Go authority could not canonicalize its Go application' >&2
    return 1
  fi
  executable_bytes="$(od -An -tx1 -N4 -- "$candidate" | tr -d '[:space:]')"
  if [[ "$executable_bytes" != 7f454c46 ]]; then
    echo 'WindShare Go authority accepts only a native ELF Go application' >&2
    return 1
  fi

  exec {WINDSHARE_GO_DESCRIPTOR}<"$candidate"
  declare -gr WINDSHARE_GO_DESCRIPTOR
  # A parent-stable $$ addresses the wrong descriptor table when callers settle
  # authority inside a Bash subshell; BASHPID identifies the actual FD owner.
  retained_executable="/proc/$BASHPID/fd/$WINDSHARE_GO_DESCRIPTOR"
  if [[ ! -r "$retained_executable" ]]; then
    echo 'WindShare Go authority could not retain its Go application' >&2
    return 1
  fi
  candidate_identity="$(stat -Lc '%d:%i' -- "$candidate")"
  if [[ "$(stat -Lc '%d:%i' -- "$retained_executable")" != "$candidate_identity" ]]; then
    echo 'WindShare Go application changed while authority was being settled' >&2
    return 1
  fi

  go_metadata="$(env -u GOFLAGS -u GOWORK -u GOOS -u GOARCH -u GOROOT \
    GOENV=off GOTOOLCHAIN=local "$retained_executable" env GOROOT GOHOSTOS GOHOSTARCH)" || return 1
  mapfile -t go_metadata_lines <<<"$go_metadata"
  if (( ${#go_metadata_lines[@]} != 3 )); then
    echo 'WindShare Go application returned incomplete host metadata' >&2
    return 1
  fi
  go_root="${go_metadata_lines[0]}"
  host_os="${go_metadata_lines[1]}"
  host_arch="${go_metadata_lines[2]}"
  expected_executable="$(readlink -f -- "$go_root/bin/go" 2>/dev/null || true)"
  if [[ "$expected_executable" != "$candidate" ]]; then
    echo 'WindShare Go application is not the bin/go reported by its GOROOT' >&2
    return 1
  fi
  if ! env -u GOFLAGS -u GOWORK -u GOOS -u GOARCH -u GOROOT \
    GOENV=off GOTOOLCHAIN=local "$retained_executable" version -m "$retained_executable" \
    | grep -Eq $'^[[:space:]]*path[[:space:]]+cmd/go$'; then
    echo 'WindShare Go application does not identify itself as cmd/go' >&2
    return 1
  fi
  if [[ "$host_os" != linux ]]; then
    echo "Linux validation requires a Linux Go host, received: $host_os" >&2
    return 1
  fi
  if [[ ! "$host_arch" =~ ^[a-z0-9]+$ ]]; then
    echo 'WindShare Go host architecture is invalid' >&2
    return 1
  fi

  declare -gr WINDSHARE_GO_CANDIDATE="$candidate"
  declare -gr WINDSHARE_GO_EXECUTABLE="$retained_executable"
  declare -gr WINDSHARE_GO_IDENTITY="$candidate_identity"
  declare -gr WINDSHARE_GO_HOST_OS="$host_os"
  declare -gr WINDSHARE_GO_HOST_ARCH="$host_arch"
  declare -gr WINDSHARE_GO_AUTHORITY_ACTIVE=1
  export WINDSHARE_GO_EXECUTABLE WINDSHARE_GO_HOST_OS WINDSHARE_GO_HOST_ARCH WINDSHARE_GO_AUTHORITY_ACTIVE
}

windshare_assert_go_authority_active() {
  if [[ "${WINDSHARE_GO_AUTHORITY_ACTIVE:-}" != 1 ||
    -z "${WINDSHARE_GO_EXECUTABLE:-}" || -z "${WINDSHARE_GO_IDENTITY:-}" ||
    ! -r "${WINDSHARE_GO_EXECUTABLE:-/nonexistent}" ||
    "$(stat -Lc '%d:%i' -- "$WINDSHARE_GO_EXECUTABLE" 2>/dev/null || true)" != "$WINDSHARE_GO_IDENTITY" ]]; then
    echo 'WindShare Go authority was not retained before use' >&2
    return 1
  fi
}

windshare_go() {
  windshare_assert_go_authority_active || return 1
  env -u GOROOT GOENV=off GOTOOLCHAIN=local \
    WINDSHARE_GO_EXECUTABLE="$WINDSHARE_GO_EXECUTABLE" \
    PATH="$(dirname -- "$WINDSHARE_GO_CANDIDATE"):$PATH" \
    "$WINDSHARE_GO_EXECUTABLE" "$@"
}

# Go hides stdout from successful tests in ordinary mode. Central ownership of
# `test -json` keeps scenario JSONL visible without letting callers add a second
# encoder or accidentally execute the test subcommand twice.
windshare_go_test_json() {
  local argument
  local name
  local normalized
  for name in GOFLAGS GOWORK GOOS GOARCH GOENV GOTOOLCHAIN GOROOT; do
    if [[ -v "$name" ]] && windshare_go_selection_ambient "$name"; then
      echo "$name must be absent when invoking Go JSON tests" >&2
      return 2
    fi
  done
  if (( $# == 0 )); then
    echo 'Go JSON test invocation requires an explicit test selection' >&2
    return 2
  fi
  for argument in "$@"; do
    normalized="${argument,,}"
    case "$normalized" in
      test|-json|--json|-json=*|--json=*)
        echo "Go JSON test invocation owns the $argument argument" >&2
        return 2
        ;;
    esac
  done
  windshare_go test -json "$@"
}

windshare_go_consumer() {
  windshare_assert_go_authority_active || return 1
  if (( $# == 0 )); then
    echo 'WindShare Go consumer requires a command' >&2
    return 2
  fi
  env -u GOROOT GOENV=off GOTOOLCHAIN=local \
    WINDSHARE_GO_EXECUTABLE="$WINDSHARE_GO_EXECUTABLE" \
    PATH="$(dirname -- "$WINDSHARE_GO_CANDIDATE"):$PATH" \
    "$@"
}
