#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
export WS_TEST_ROOT="$work/host" WS_TEST_CONFIG="$work/node.json" WS_TEST_LOG="$work/restarts" WS_TEST_UNIT_LOG="$work/systemctl" WS_TEST_FAILED="$work/unit-failed"
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
case "$1" in
    reset-failed)
        rm -f -- "$WS_TEST_FAILED"
        echo "reset-failed" >> "$WS_TEST_UNIT_LOG"
        exit 0
        ;;
    stop)
        target=$(basename "$(readlink "$WS_TEST_ROOT/current")")
        [[ -x $WS_TEST_ROOT/releases/$target/wsrelay ]]
        echo "stop:$target" >> "$WS_TEST_UNIT_LOG"
        exit 0
        ;;
    restart)
        [[ ! -e $WS_TEST_FAILED ]]
        target=$(basename "$(readlink "$WS_TEST_ROOT/current")")
        echo "restart:$target" >> "$WS_TEST_UNIT_LOG"
        echo "$target" >> "$WS_TEST_LOG"
        [[ $target != "${FAIL_RESTART:-}" ]]
        ;;
    *)
        exit 1
        ;;
esac
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
last_healthy() { basename "$(readlink "$WS_TEST_ROOT/last-healthy")"; }
fails() { if "$@"; then echo "unexpected success: $*" >&2; exit 1; fi; }

for failure in FAIL_RESTART FAIL_HEALTH; do
    : > "$WS_TEST_UNIT_LOG"
    export "$failure=$(revision 1)"
    fails install_node 1
    unset "$failure"
    [[ ! -e $WS_TEST_ROOT/current && ! -L $WS_TEST_ROOT/current && ! -e $WS_TEST_ROOT/last-healthy && ! -L $WS_TEST_ROOT/last-healthy && ! -d $WS_TEST_ROOT/releases/$(revision 1) ]]
    [[ $(cat "$WS_TEST_UNIT_LOG") == $'reset-failed\nrestart:'"$(revision 1)"$'\nstop:'"$(revision 1)"$'\nreset-failed' ]]
done
install_node 2
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" ]]
fails install_node 3 "$(printf '%064d' 0)"
FAIL_CONFIG=1 fails install_node 3
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && ! -d $WS_TEST_ROOT/releases/$(revision 3) ]]
for failure in FAIL_RESTART FAIL_HEALTH; do
    export "$failure=$(revision 3)"
    fails install_node 3
    unset "$failure"
    [[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && $(tail -2 "$WS_TEST_LOG") == "$(revision 3)"$'\n'"$(revision 2)" ]]
done
# A bad rollback must also surface as a failed installation and discard the failed release.
FAIL_RESTART=$(revision 2) FAIL_HEALTH=$(revision 3) fails install_node 3
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && ! -d $WS_TEST_ROOT/releases/$(revision 3) ]]
# Clear any prior start-limit/failed state before restarting a hotfix release.
touch "$WS_TEST_FAILED"
install_node 3
[[ ! -e $WS_TEST_FAILED && $(selected) == "$(revision 3)" && $(last_healthy) == "$(revision 3)" ]]
install_node 4
install_node 5
[[ $(selected) == "$(revision 5)" && $(last_healthy) == "$(revision 5)" && $(find "$WS_TEST_ROOT/releases" -mindepth 1 -maxdepth 1 -type d | wc -l) == 3 ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 4)/wsrelay ]]
# The same revision can be retried, but cannot be silently rebound.
install_node 5
echo '#other build' >> "$work/wsrelay"
fails install_node 5 "$(sha256sum "$work/wsrelay" | cut -d' ' -f1)"
[[ $(selected) == "$(revision 5)" && $(last_healthy) == "$(revision 5)" ]]
# Restore the original binary content for subsequent interruption and migration cases.
sed -i '$d' "$work/wsrelay"

# Interrupting an upgrade after switching current leaves current on the unverified candidate
# while last-healthy keeps pointing to the last verified release (revision 5).
mkdir -p "$WS_TEST_ROOT/releases/$(revision 6)"
cp "$work/wsrelay" "$WS_TEST_ROOT/releases/$(revision 6)/wsrelay"
chmod 0755 "$WS_TEST_ROOT/releases/$(revision 6)/wsrelay"
ln -sfn "$WS_TEST_ROOT/releases/$(revision 6)" "$WS_TEST_ROOT/current"
# Retrying the same failing candidate must still roll back to last-healthy (revision 5).
FAIL_HEALTH=$(revision 6) fails install_node 6
[[ $(selected) == "$(revision 5)" && $(last_healthy) == "$(revision 5)" && $(tail -2 "$WS_TEST_LOG") == "$(revision 6)"$'\n'"$(revision 5)" ]]
# Deploying another failing hotfix after an interrupted switch must also roll back to revision 5, not 6.
ln -sfn "$WS_TEST_ROOT/releases/$(revision 6)" "$WS_TEST_ROOT/current"
FAIL_HEALTH=$(revision 7) fails install_node 7
[[ $(selected) == "$(revision 5)" && $(last_healthy) == "$(revision 5)" && $(tail -2 "$WS_TEST_LOG") == "$(revision 7)"$'\n'"$(revision 5)" ]]
[[ ! -d $WS_TEST_ROOT/releases/$(revision 7) ]]
# When a hotfix succeeds after an interrupted switch, retention must pin last-healthy (5), not unverified 6.
ln -sfn "$WS_TEST_ROOT/releases/$(revision 6)" "$WS_TEST_ROOT/current"
touch -t 202001010000 "$WS_TEST_ROOT/releases/$(revision 5)"
touch -t 202001020000 "$WS_TEST_ROOT/releases/$(revision 4)"
touch -t 202001030000 "$WS_TEST_ROOT/releases/$(revision 6)"
touch -t 202001040000 "$WS_TEST_ROOT/releases/$(revision 3)"
install_node 7
[[ $(selected) == "$(revision 7)" && $(last_healthy) == "$(revision 7)" ]]
[[ $(find "$WS_TEST_ROOT/releases" -mindepth 1 -maxdepth 1 -type d | wc -l) == 3 ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 5)/wsrelay && ! -d $WS_TEST_ROOT/releases/$(revision 6) ]]

# A legacy host without last-healthy bootstraps it from a healthy current release only after preflight.
rm -rf -- "$WS_TEST_ROOT"
install_node 2
rm -- "$WS_TEST_ROOT/last-healthy"
FAIL_CONFIG=1 fails install_node 3
[[ ! -e $WS_TEST_ROOT/last-healthy && ! -L $WS_TEST_ROOT/last-healthy ]]
FAIL_HEALTH=$(revision 3) fails install_node 3
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && $(tail -2 "$WS_TEST_LOG") == "$(revision 3)"$'\n'"$(revision 2)" ]]

# An interrupted first install has no last-healthy and unhealthy current; retrying and failing must stop and clear current.
rm -rf -- "$WS_TEST_ROOT"
mkdir -p "$WS_TEST_ROOT/releases/$(revision 1)"
cp "$work/wsrelay" "$WS_TEST_ROOT/releases/$(revision 1)/wsrelay"
chmod 0755 "$WS_TEST_ROOT/releases/$(revision 1)/wsrelay"
ln -sfn "$WS_TEST_ROOT/releases/$(revision 1)" "$WS_TEST_ROOT/current"
: > "$WS_TEST_UNIT_LOG"
FAIL_HEALTH=$(revision 1) fails install_node 1
[[ ! -e $WS_TEST_ROOT/current && ! -L $WS_TEST_ROOT/current && ! -e $WS_TEST_ROOT/last-healthy && ! -L $WS_TEST_ROOT/last-healthy ]]
[[ $(cat "$WS_TEST_UNIT_LOG") == $'reset-failed\nrestart:'"$(revision 1)"$'\nstop:'"$(revision 1)"$'\nreset-failed' ]]
echo "wsrelay install, preflight, rollback and retention passed"
