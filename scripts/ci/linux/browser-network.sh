#!/usr/bin/env bash
# The public browser graph consumes only immutable completion evidence. The
# protected workflow supplies its target SHA; focused local consumption derives
# the checked-out commit so both paths bind the same consumer API.
set -euo pipefail

if (( $# != 0 )); then
  echo 'browser-network.sh does not accept positional operands' >&2
  exit 2
fi

script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd -- "$script_directory/../../.." && pwd -P)"
cd "$repository_root"

if [[ -z "${BROWSER_NETWORK_COMPLETION:-}" ]]; then
  BROWSER_NETWORK_COMPLETION="$repository_root/test-results/browser-network-completion.json"
  export BROWSER_NETWORK_COMPLETION
fi

target_sha_source=environment
if [[ -z "${WINDSHARE_TARGET_SHA:-}" ]]; then
  WINDSHARE_TARGET_SHA="$(git rev-parse --verify HEAD)"
  export WINDSHARE_TARGET_SHA
  target_sha_source=checkout
fi
if [[ ! "$WINDSHARE_TARGET_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  echo 'browser network target SHA must be an exact lowercase commit identity' >&2
  exit 1
fi

echo "== browser-network: target_sha=$WINDSHARE_TARGET_SHA source=$target_sha_source =="
exec node --experimental-strip-types scripts/ci/browsergate/network-completion.mjs consume
