#!/usr/bin/env bash

# Git's process environment can redirect even a command that uses -C. Release
# evidence therefore clears the whole Git namespace before naming its repository
# and worktree explicitly.
WINDSHARE_CORE_RELEASE_VERIFIER_PATHS=(
  go.mod
  go.sum
  scripts/ci/_coremodulezip/main.go
  scripts/ci/core-release-linux-native.sh
  scripts/ci/core-release-linux-native-root.sh
  scripts/ci/core-release-windows-native.psm1
  scripts/ci/core-release-windows-native-worker.ps1
)

windshare_run_isolated_git() (
  local variable_name

  while IFS='=' read -r variable_name _; do
    case "$variable_name" in
      GIT_*) unset "$variable_name" ;;
    esac
  done < <(env)
  export GIT_CONFIG_NOSYSTEM=1
  export GIT_CONFIG_GLOBAL=/dev/null
  export GIT_NO_REPLACE_OBJECTS=1
  export GIT_TERMINAL_PROMPT=0
  export LC_ALL=C
  git "$@"
)

windshare_assert_exact_release_checkout() {
  local repository_root="$1"
  local expected_commit="$2"
  local actual_commit
  local object_type
  local status
  local index_entries
  local index_entry
  local index_view
  local git_directory
  local git_arguments

  if [[ ! "$expected_commit" =~ ^[0-9a-f]{40}$ ]]; then
    echo "release checkout requires an exact lowercase 40-character SHA" >&2
    return 1
  fi
  if [ -z "$repository_root" ] || [ ! -d "$repository_root" ] || [ -L "$repository_root" ]; then
    echo "release checkout requires an existing no-follow repository root" >&2
    return 1
  fi
  repository_root="$(cd -- "$repository_root" && pwd -P)" || return 1
  git_directory="$repository_root/.git"
  if [ ! -d "$git_directory" ] || [ -L "$git_directory" ]; then
    echo "release checkout requires a standalone no-follow Git directory" >&2
    return 1
  fi
  git_arguments=(
    --git-dir="$git_directory"
    --work-tree="$repository_root"
    -c core.fsmonitor=false
    -c core.untrackedCache=false
  )

  actual_commit="$(windshare_run_isolated_git "${git_arguments[@]}" rev-parse HEAD)" || return 1
  object_type="$(windshare_run_isolated_git "${git_arguments[@]}" cat-file -t "$expected_commit")" || return 1
  if [ "$actual_commit" != "$expected_commit" ] || [ "$object_type" != "commit" ]; then
    echo "release checkout does not directly equal commit $expected_commit" >&2
    return 1
  fi

  # Porcelain deliberately trusts these index bits. Rejecting every non-default
  # tag keeps assume-unchanged, skip-worktree, and fsmonitor state from hiding a
  # verifier mutation after its tests have executed.
  for index_view in -v -f; do
    index_entries="$(windshare_run_isolated_git "${git_arguments[@]}" ls-files "$index_view")" || return 1
    while IFS= read -r index_entry; do
      [ -z "$index_entry" ] && continue
      case "$index_entry" in
        "H "*) ;;
        *)
          echo "release checkout has non-default Git index state: $index_entry" >&2
          return 1
          ;;
      esac
    done <<<"$index_entries"
  done

  status="$(windshare_run_isolated_git "${git_arguments[@]}" status --porcelain=v1 --untracked-files=all)" || return 1
  if [ -n "$status" ]; then
    echo "release checkout is not clean:" >&2
    printf '%s\n' "$status" >&2
    return 1
  fi
}

windshare_assert_exact_release_file_projection() {
  local repository_root="$1"
  local expected_commit="$2"
  local relative_path
  local expected_object
  local actual_object

  shift 2
  if [ "$#" -eq 0 ]; then
    echo "exact release projection requires at least one verifier path" >&2
    return 1
  fi
  for relative_path in "$@"; do
    case "$relative_path" in
      ""|/*|*\\*|../*|*/../*|*/..)
        echo "invalid exact release verifier path: $relative_path" >&2
        return 1
        ;;
    esac
    if [ ! -f "$repository_root/$relative_path" ] || [ -L "$repository_root/$relative_path" ]; then
      echo "exact release verifier input is not a regular file: $relative_path" >&2
      return 1
    fi
    expected_object="$(windshare_run_isolated_git \
      --git-dir="$repository_root/.git" \
      rev-parse --verify "$expected_commit:$relative_path")" || return 1
    actual_object="$(windshare_run_isolated_git hash-object --no-filters -- \
      "$repository_root/$relative_path")" || return 1
    if [ "$actual_object" != "$expected_object" ]; then
      echo "exact release verifier input differs from its commit blob: $relative_path" >&2
      return 1
    fi
  done
}

windshare_create_exact_release_checkout() {
  local source_repository="$1"
  local expected_commit="$2"
  local destination="$3"

  shift 3
  if [[ ! "$expected_commit" =~ ^[0-9a-f]{40}$ ]]; then
    echo "exact release checkout requires an exact lowercase 40-character SHA" >&2
    return 1
  fi
  if [ "$#" -eq 0 ]; then
    echo "exact release checkout requires verifier projection paths" >&2
    return 1
  fi
  if [ -z "$destination" ] || [ -e "$destination" ] || [ -L "$destination" ]; then
    echo "exact release checkout destination must not exist: $destination" >&2
    return 1
  fi
  windshare_run_isolated_git clone --quiet --no-hardlinks --no-checkout -- \
    "$source_repository" "$destination" || return 1
  windshare_run_isolated_git \
    --git-dir="$destination/.git" \
    --work-tree="$destination" \
    -c core.fsmonitor=false \
    -c core.untrackedCache=false \
    checkout --quiet --detach "$expected_commit" || return 1
  windshare_assert_exact_release_checkout "$destination" "$expected_commit" || return 1
  windshare_assert_exact_release_file_projection \
    "$destination" \
    "$expected_commit" \
    "$@"
}
