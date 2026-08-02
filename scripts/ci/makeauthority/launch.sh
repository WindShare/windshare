#!/usr/bin/env bash
set -euo pipefail

if (( $# < 4 )); then
  echo 'WindShare Make launcher requires its retained authorities and one target' >&2
  exit 2
fi
retained_makefile="$1"
expected_makefile_sha256="$2"
repository_root="$3"
shift 3
source "${BASH_SOURCE[0]%/*}/authority.sh"
if [[ ! "${BASH_VERSION:-}" =~ ^[0-9]+\.[0-9]+ ]] || [[ ! -x "/proc/$$/exe" ]]; then
  echo 'WindShare Make launcher requires its retained Bash interpreter' >&2
  exit 1
fi
readonly retained_bash="/proc/$$/exe"
windshare_enter_make_authority
windshare_enter_makefile_authority "$retained_makefile" "$expected_makefile_sha256"
windshare_enter_git_authority
checkout_commit="$(windshare_git_head_commit "$repository_root")"
windshare_make -f "$WINDSHARE_RETAINED_MAKEFILE" \
  'WINDSHARE_HOST_GOOS=linux' \
  "WINDSHARE_CORE_ARTIFACT_COMMIT_SHA=$checkout_commit" \
  "WINDSHARE_RETAINED_MAKEFILE=$WINDSHARE_RETAINED_MAKEFILE" \
  "WINDSHARE_RECIPE_SHELL=$retained_bash" \
  "WINDSHARE_BASH_EXECUTABLE=$retained_bash" \
  "$@"
