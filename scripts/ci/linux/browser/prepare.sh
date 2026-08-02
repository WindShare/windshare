#!/usr/bin/env bash
# Helper construction is deliberately separated from the OIDC-bearing job. It
# may execute repository and third-party build code, but cannot inherit either
# the runner request bearer or a minted workload assertion.
set -euo pipefail

if (( $# != 0 )); then
  echo 'browser network prepare does not accept positional operands' >&2
  exit 2
fi

helper_directory="${BROWSER_NETWORK_HELPER_DIRECTORY:-}"
runner_temporary="${RUNNER_TEMP:-}"
github_run_id="${GITHUB_RUN_ID:-}"
github_run_attempt="${GITHUB_RUN_ATTEMPT:-}"
unset BROWSER_NETWORK_HELPER_DIRECTORY

if [[ ! "$github_run_id" =~ ^[1-9][0-9]{0,19}$ || ! "$github_run_attempt" =~ ^[1-9][0-9]{0,19}$ ]]; then
  echo 'browser network workflow identity is invalid' >&2
  exit 1
fi
if [[ -z "$runner_temporary" || ! -d "$runner_temporary" || -L "$runner_temporary" ]]; then
  echo 'browser network runner temporary authority is invalid' >&2
  exit 1
fi
runner_temporary="$(realpath -e -- "$runner_temporary")"
expected_helper_directory="$runner_temporary/windshare-browser-network-helpers-$github_run_id-$github_run_attempt"
if [[ "$helper_directory" != "$expected_helper_directory" || -e "$helper_directory" || -L "$helper_directory" ]]; then
  echo 'browser network helper output must be the exact new workflow-owned directory' >&2
  exit 1
fi

script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd -- "$script_directory/../../../.." && pwd -P)"
cd "$repository_root"
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority

echo "== browser-network-prepare: run_id=$github_run_id attempt=$github_run_attempt =="
windshare_go_consumer pnpm -C web build:browser-network-matrix-helpers -- "$helper_directory"

if [[ ! -d "$helper_directory" || -L "$helper_directory" || "$(realpath -e -- "$helper_directory")" != "$expected_helper_directory" ]]; then
  echo 'browser network helper build replaced its directory authority' >&2
  exit 1
fi
for helper in helper-manifest.json browsermatrixpublish testprocessowner; do
  helper_path="$helper_directory/$helper"
  if [[ ! -f "$helper_path" || -L "$helper_path" ]]; then
    echo "browser network helper artifact is invalid: $helper" >&2
    exit 1
  fi
done
if [[ ! -x "$helper_directory/browsermatrixpublish" || ! -x "$helper_directory/testprocessowner" ]]; then
  echo 'browser network executable helpers lack execute authority' >&2
  exit 1
fi
echo '== browser-network-prepare: PASS =='
