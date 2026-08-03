#!/usr/bin/env bash
# The public browser graph consumes only immutable completion evidence. Keeping
# JWT minting and network execution outside this process tree makes the same Make
# target safe for hosted CI and explicit local artifact verification.
set -euo pipefail

if (( $# != 0 )); then
  echo 'browser-network.sh does not accept positional operands' >&2
  exit 2
fi

script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd -- "$script_directory/../../.." && pwd -P)"
cd "$repository_root"
if [[ -z "${BROWSER_NETWORK_COMPLETION:-}" ]]; then
  # Public Make owns the canonical local path. Hosted full validation replaces
  # it only with the retained descriptor path minted by the protected launcher.
  BROWSER_NETWORK_COMPLETION="$repository_root/test-results/browser-network-completion.json"
  export BROWSER_NETWORK_COMPLETION
fi
exec node --experimental-strip-types scripts/ci/browsergate/network-completion.mjs consume
