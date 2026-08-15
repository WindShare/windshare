#!/usr/bin/env bash

# Git's process environment can redirect even a command that uses -C. Release
# evidence therefore clears the whole Git namespace before naming its repository
# and worktree explicitly.
WINDSHARE_RELEASE_VERIFIER_PATHS=(
  go.mod
  go.sum
  scripts/ci/_modulezip/main.go
  scripts/ci/native-output/linux/certify.sh
  scripts/ci/native-output/linux/root-worker.sh
  scripts/ci/native-output/windows/certify.psm1
  scripts/ci/native-output/windows/worker.ps1
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

windshare_resolve_release_repository() {
  local repository_root="$1"
  local git_entry
  local inside_worktree
  local top_level
  local git_directory
  local common_directory
  local index_path
  local index_parent

  if [ -z "$repository_root" ] || [ ! -d "$repository_root" ] || [ -L "$repository_root" ]; then
    echo "release checkout requires an existing no-follow repository root" >&2
    return 1
  fi
  repository_root="$(cd -- "$repository_root" && pwd -P)" || return 1
  git_entry="$repository_root/.git"
  if [ -L "$git_entry" ] || { [ ! -d "$git_entry" ] && [ ! -f "$git_entry" ]; }; then
    echo "release checkout requires a no-follow Git metadata entry" >&2
    return 1
  fi

  inside_worktree="$(windshare_run_isolated_git -C "$repository_root" \
    -c core.fsmonitor=false -c core.untrackedCache=false \
    rev-parse --is-inside-work-tree)" || return 1
  if [ "$inside_worktree" != true ]; then
    echo "release checkout repository root is not inside a Git worktree" >&2
    return 1
  fi
  top_level="$(windshare_run_isolated_git -C "$repository_root" \
    -c core.fsmonitor=false -c core.untrackedCache=false \
    rev-parse --show-toplevel)" || return 1
  if [ -z "$top_level" ] || [ ! -d "$top_level" ] || [ -L "$top_level" ]; then
    echo "release checkout Git top-level is not an existing no-follow directory" >&2
    return 1
  fi
  top_level="$(cd -- "$top_level" && pwd -P)" || return 1
  if [ "$top_level" != "$repository_root" ]; then
    echo "release checkout top-level is $top_level instead of $repository_root" >&2
    return 1
  fi

  git_directory="$(windshare_run_isolated_git -C "$repository_root" \
    rev-parse --absolute-git-dir)" || return 1
  if [ -z "$git_directory" ] || [ ! -d "$git_directory" ] || [ -L "$git_directory" ]; then
    echo "release checkout Git directory is not an existing no-follow directory" >&2
    return 1
  fi
  git_directory="$(cd -- "$git_directory" && pwd -P)" || return 1
  common_directory="$(windshare_run_isolated_git -C "$repository_root" \
    rev-parse --path-format=absolute --git-common-dir)" || return 1
  if [ -z "$common_directory" ] || [ ! -d "$common_directory" ] || [ -L "$common_directory" ]; then
    echo "release checkout common Git directory is not an existing no-follow directory" >&2
    return 1
  fi
  common_directory="$(cd -- "$common_directory" && pwd -P)" || return 1
  index_path="$(windshare_run_isolated_git -C "$repository_root" \
    rev-parse --path-format=absolute --git-path index)" || return 1
  if [ -z "$index_path" ] || [ ! -f "$index_path" ] || [ -L "$index_path" ]; then
    echo "release checkout index is not an existing no-follow file" >&2
    return 1
  fi
  index_parent="$(cd -- "$(dirname -- "$index_path")" && pwd -P)" || return 1

  WINDSHARE_RELEASE_REPOSITORY_ROOT="$repository_root"
  WINDSHARE_RELEASE_GIT_DIRECTORY="$git_directory"
  WINDSHARE_RELEASE_COMMON_DIRECTORY="$common_directory"
  WINDSHARE_RELEASE_INDEX_PATH="$index_parent/$(basename -- "$index_path")"
}

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
  windshare_resolve_release_repository "$repository_root" || return 1
  repository_root="$WINDSHARE_RELEASE_REPOSITORY_ROOT"
  git_directory="$WINDSHARE_RELEASE_GIT_DIRECTORY"
  # The resolved per-worktree Git directory binds every check to the index that
  # was inspected above; the common object store is validated but never guessed.
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
  local git_directory

  shift 2
  windshare_resolve_release_repository "$repository_root" || return 1
  repository_root="$WINDSHARE_RELEASE_REPOSITORY_ROOT"
  git_directory="$WINDSHARE_RELEASE_GIT_DIRECTORY"
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
      --git-dir="$git_directory" \
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
  windshare_resolve_release_repository "$source_repository" || return 1
  source_repository="$WINDSHARE_RELEASE_REPOSITORY_ROOT"
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
