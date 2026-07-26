#!/usr/bin/env bash

# Linux native certification walks from the filesystem root, so choosing a
# private leaf beneath /tmp cannot repair unsafe sticky ancestry. Release gates
# instead allocate beside the checkout after proving the complete path.

windshare_linux_physical_directory() {
  local directory="$1"
  if [ ! -d "$directory" ] || [ -L "$directory" ]; then
    echo "Linux release temp path is not a no-follow directory: $directory" >&2
    return 1
  fi
  (cd -- "$directory" && pwd -P)
}

windshare_linux_assert_ext4_ancestry() {
  local candidate="$1"
  local label="$2"
  local receiver_uid
  local resolved
  local current
  local owner_uid
  local mode
  local mode_value
  local filesystem_magic

  if [ "$(uname -s)" != "Linux" ]; then
    echo "$label is meaningful only on Linux" >&2
    return 1
  fi
  receiver_uid="$(id -u)"
  if [ "$receiver_uid" -eq 0 ]; then
    echo "$label must be owned and exercised by an unprivileged receiver" >&2
    return 1
  fi
  resolved="$(windshare_linux_physical_directory "$candidate")" || return 1
  owner_uid="$(stat -c '%u' -- "$resolved")" || return 1
  if [ "$owner_uid" -ne "$receiver_uid" ]; then
    echo "$label is not owned by the receiver: $resolved" >&2
    return 1
  fi

  current="$resolved"
  while :; do
    if [ ! -d "$current" ] || [ -L "$current" ]; then
      echo "$label ancestry is not a no-follow directory: $current" >&2
      return 1
    fi
    owner_uid="$(stat -c '%u' -- "$current")" || return 1
    if [ "$owner_uid" -ne 0 ] && [ "$owner_uid" -ne "$receiver_uid" ]; then
      echo "$label ancestry is owned by another unprivileged user: $current" >&2
      return 1
    fi
    mode="$(stat -c '%a' -- "$current")" || return 1
    if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
      echo "$label ancestry has an unparseable mode at $current: $mode" >&2
      return 1
    fi
    mode_value=$((8#$mode))
    if (( (mode_value & 0022) != 0 )); then
      echo "$label ancestry grants group/other write authority: $current ($mode)" >&2
      return 1
    fi
    filesystem_magic="$(stat -f -c '%t' -- "$current")" || return 1
    if [ "${filesystem_magic,,}" != "ef53" ]; then
      echo "$label ancestry is not ext4 at $current (magic $filesystem_magic)" >&2
      return 1
    fi
    if [ "$current" = "/" ]; then
      break
    fi
    current="$(dirname -- "$current")"
  done

  printf '%s\n' "$resolved"
}

windshare_linux_assert_private_directory() {
  local candidate="$1"
  local label="$2"
  local receiver_uid
  local owner_uid
  local mode

  receiver_uid="$(id -u)"
  owner_uid="$(stat -c '%u' -- "$candidate")" || return 1
  mode="$(stat -c '%a' -- "$candidate")" || return 1
  if [ "$owner_uid" -ne "$receiver_uid" ] || [ "$mode" != "700" ]; then
    echo "$label must be receiver-owned with exact mode 0700: $candidate (uid $owner_uid mode $mode)" >&2
    return 1
  fi
}

windshare_linux_assert_owned_release_root() {
  local candidate="$1"
  local expected_parent="$2"
  local resolved_candidate
  local resolved_parent
  local candidate_name

  resolved_parent="$(windshare_linux_physical_directory "$expected_parent")" || return 1
  resolved_candidate="$(windshare_linux_physical_directory "$candidate")" || return 1
  candidate_name="$(basename -- "$resolved_candidate")"
  if [ "$(dirname -- "$resolved_candidate")" != "$resolved_parent" ] ||
     [[ ! "$candidate_name" =~ ^\.windshare-core-release\.[A-Za-z0-9]+$ ]]; then
    echo "refusing unowned Linux release temp root: $resolved_candidate" >&2
    return 1
  fi
  windshare_linux_assert_private_directory "$resolved_candidate" "Linux release temp root" || return 1
  printf '%s\n' "$resolved_candidate"
}

windshare_linux_remove_release_root() {
  local candidate="$1"
  local expected_parent="$2"
  local resolved_candidate

  if [ ! -e "$candidate" ] && [ ! -L "$candidate" ]; then
    return 0
  fi
  if [ -e "$candidate/.windshare-preserve-native-fixture" ]; then
    echo "refusing to remove a Linux native fixture with unproven loop detachment: $candidate" >&2
    return 1
  fi
  windshare_linux_assert_ext4_ancestry \
    "$expected_parent" "Linux release cleanup parent" >/dev/null || return 1
  resolved_candidate="$(
    windshare_linux_assert_owned_release_root "$candidate" "$expected_parent"
  )" || return 1
  windshare_linux_assert_ext4_ancestry \
    "$resolved_candidate" "Linux release cleanup root" >/dev/null || return 1
  rm -rf -- "$resolved_candidate"
}

windshare_linux_prepare_release_environment() {
  local repository_root="$1"
  local workspace_root
  local release_root
  local unresolved_release_root
  local test_temp

  workspace_root="$(dirname -- "$repository_root")"
  workspace_root="$(
    windshare_linux_assert_ext4_ancestry "$workspace_root" "Linux release workspace"
  )" || return 1
  unresolved_release_root="$(
    mktemp -d "$workspace_root/.windshare-core-release.XXXXXXXX"
  )" || return 1
  chmod 0700 -- "$unresolved_release_root" || {
    windshare_linux_remove_release_root "$unresolved_release_root" "$workspace_root" || true
    return 1
  }
  release_root="$(
    windshare_linux_assert_owned_release_root "$unresolved_release_root" "$workspace_root"
  )" || {
    windshare_linux_remove_release_root "$unresolved_release_root" "$workspace_root" || true
    return 1
  }
  if ! windshare_linux_assert_ext4_ancestry "$release_root" "Linux release temp root" >/dev/null; then
    windshare_linux_remove_release_root "$release_root" "$workspace_root" || true
    return 1
  fi

  test_temp="$release_root/tmp"
  install -d -m 0700 -- "$test_temp" || {
    windshare_linux_remove_release_root "$release_root" "$workspace_root" || true
    return 1
  }
  windshare_linux_assert_private_directory "$test_temp" "Linux release TMPDIR" || {
    windshare_linux_remove_release_root "$release_root" "$workspace_root" || true
    return 1
  }

  WINDSHARE_LINUX_RELEASE_TEMP_PARENT="$workspace_root"
  WINDSHARE_LINUX_RELEASE_TEMP_ROOT="$release_root"
  TMPDIR="$test_temp"
  export WINDSHARE_LINUX_RELEASE_TEMP_PARENT WINDSHARE_LINUX_RELEASE_TEMP_ROOT TMPDIR
}

windshare_linux_cleanup_release_environment() {
  if [ -z "${WINDSHARE_LINUX_RELEASE_TEMP_ROOT:-}" ] ||
     [ -z "${WINDSHARE_LINUX_RELEASE_TEMP_PARENT:-}" ]; then
    echo "Linux release temp ownership was not established" >&2
    return 1
  fi
  windshare_linux_remove_release_root \
    "$WINDSHARE_LINUX_RELEASE_TEMP_ROOT" \
    "$WINDSHARE_LINUX_RELEASE_TEMP_PARENT"
}
