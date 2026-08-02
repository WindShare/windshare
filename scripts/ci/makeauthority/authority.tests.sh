#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../../.."
fixture="$(mktemp -d)"
trap 'rm -rf -- "$fixture"' EXIT

for name in MAKEFLAGS MFLAGS GNUMAKEFLAGS MAKEFILES; do
  unset "$name"
done

if (
  source scripts/ci/makeauthority/authority.sh
  windshare_enter_make_authority /bin/true
) >/dev/null 2>&1; then
  echo 'native non-Make application was accepted' >&2
  exit 1
fi

cp -- "$(type -P make)" "$fixture/make"
chmod +x "$fixture/make"
(
  source scripts/ci/makeauthority/authority.sh
  windshare_enter_make_authority "$fixture/make"
  mv -- "$fixture/make" "$fixture/original-make"
  printf '#!/usr/bin/env bash\nexit 99\n' >"$fixture/make"
  chmod +x "$fixture/make"
  version="$(windshare_make --version | head -n 1)"
  [[ "$version" == GNU\ Make\ * ]]
)

echo 'make authority Bash tests: PASS'
