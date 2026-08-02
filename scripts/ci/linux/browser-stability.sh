#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")/../../.." || exit 1
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority || exit 1

windshare_go_consumer node scripts/ci/browsergate/main.mjs local --run-policy stability
exit $?
