#!/usr/bin/env bash

windshare_load_go_package_set() {
  local requested_set="${1:?package set is required}"
  local package_output

  # Capturing first preserves the go-list failure status; process substitution
  # would let an empty set silently reach a validation command.
  package_output="$(go run ./scripts/ci/_gopackages -set "$requested_set")"
  if [[ -z "$package_output" ]]; then
    echo "package set $requested_set is empty" >&2
    return 1
  fi
  mapfile -t WINDSHARE_GO_PACKAGES <<<"$package_output"
}
