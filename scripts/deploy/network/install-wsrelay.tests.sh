#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
export WS_TEST_ROOT="$work/host" WS_TEST_CONFIG="$work/node.json" WS_TEST_LOG="$work/restarts"
mkdir -p "$work/bin"
touch "$WS_TEST_CONFIG"
sed -e "s|^readonly root=.*|readonly root=$WS_TEST_ROOT|" \
    -e "s|^readonly config=.*|readonly config=$WS_TEST_CONFIG|" "$here/install-wsrelay.sh" > "$work/install"
export PATH="$work/bin:$PATH"

cat > "$work/bin/id" <<'SH'
#!/usr/bin/env bash
[[ $1 == -u ]] && echo 0
SH
cat > "$work/bin/runuser" <<'SH'
#!/usr/bin/env bash
set -eu
[[ ! -e /proc/$$/fd/9 && $1 == -u && $2 == windshare-network && $3 == -- ]]
shift 3
exec "$@"
SH
cat > "$work/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -eu
[[ ! -e /proc/$$/fd/9 && $# == 2 && $2 == windshare-network.service ]]
[[ $1 == restart ]] || { [[ $1 == reset-failed ]]; exit; }
target=$(basename "$(readlink "$WS_TEST_ROOT/current")")
echo "$target" >> "$WS_TEST_LOG"
[[ $target != "${FAIL_RESTART:-}" ]]
SH
cat > "$work/bin/curl" <<'SH'
#!/usr/bin/env bash
[[ ${*: -1} == http://127.0.0.1:8484/healthz ]] || exit 2
target=$(basename "$(readlink "$WS_TEST_ROOT/current")")
if [[ $target == "${FAIL_HEALTH:-}" ]]; then echo 503; else echo 200; fi
SH
cat > "$work/bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat > "$work/wsrelay" <<'SH'
#!/usr/bin/env bash
[[ $# == 2 && $1 == --check-config && $2 == "$WS_TEST_CONFIG" && -f $2 && -z ${FAIL_CONFIG:-} ]]
SH
chmod +x "$work"/bin/*
digest=$(sha256sum "$work/wsrelay" | cut -d' ' -f1)
revision() { printf '%040x' "$1"; }
install_node() { bash "$work/install" "$(revision "$1")" "${2:-$digest}" < "$work/wsrelay"; }
selected() { basename "$(readlink "$WS_TEST_ROOT/current")"; }
fails() { if "$@"; then echo "unexpected success: $*" >&2; exit 1; fi; }

FAIL_RESTART=$(revision 1) fails install_node 1
[[ ! -e $WS_TEST_ROOT/current && ! -L $WS_TEST_ROOT/current && ! -d $WS_TEST_ROOT/releases/$(revision 1) ]]
install_node 2
fails install_node 3 "$(printf '%064d' 0)"
FAIL_CONFIG=1 fails install_node 3
[[ $(selected) == "$(revision 2)" && ! -d $WS_TEST_ROOT/releases/$(revision 3) ]]
for failure in FAIL_RESTART FAIL_HEALTH; do
    export "$failure=$(revision 3)"
    fails install_node 3
    unset "$failure"
    [[ $(selected) == "$(revision 2)" && $(tail -2 "$WS_TEST_LOG") == "$(revision 3)"$'\n'"$(revision 2)" ]]
done
# A bad rollback must also surface as a failed installation.
FAIL_RESTART=$(revision 2) FAIL_HEALTH=$(revision 3) fails install_node 3
[[ $(selected) == "$(revision 2)" ]]
install_node 3
install_node 4
install_node 5
[[ $(selected) == "$(revision 5)" && $(find "$WS_TEST_ROOT/releases" -mindepth 1 -maxdepth 1 -type d | wc -l) == 3 ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 4)/wsrelay ]]
# The same revision can be retried, but cannot be silently rebound.
install_node 5
echo '#other build' >> "$work/wsrelay"
fails install_node 5 "$(sha256sum "$work/wsrelay" | cut -d' ' -f1)"
[[ $(selected) == "$(revision 5)" ]]
echo "wsrelay install, preflight, rollback and retention passed"
