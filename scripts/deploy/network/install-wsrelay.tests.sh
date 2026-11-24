#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
export WS_TEST_ROOT="$work/host" WS_TEST_CONFIG="$work/node.json" WS_TEST_LOG="$work/restarts" WS_TEST_UNIT_LOG="$work/systemctl" WS_TEST_FAILED="$work/unit-failed" WS_TEST_UNIT_STATE="$work/unit-state"
mkdir -p "$work/bin"
touch "$WS_TEST_CONFIG"
real_rmdir=$(type -P rmdir)
real_readlink=$(type -P readlink)
sed -e "s|^readonly root=.*|readonly root=$WS_TEST_ROOT|" \
    -e "s|^readonly config=.*|readonly config=$WS_TEST_CONFIG|" "$here/install-wsrelay.sh" > "$work/install"
export PATH="$work/bin:$PATH"

cat > "$work/bin/id" <<'SH'
#!/usr/bin/env bash
[[ $1 == -u ]] && echo 0
SH
cat > "$work/bin/readlink" <<SH
#!/usr/bin/env bash
set -eu
if [[ \$# == 1 && \$1 =~ ^/proc/([1-9][0-9]*)/exe\$ ]]; then
    pid=\${BASH_REMATCH[1]}
    [[ -f "\$WS_TEST_UNIT_STATE" ]] || exit 1
    source "\$WS_TEST_UNIT_STATE"
    [[ \${unit_pid:-0} == "\$pid" && -n \${unit_exe:-} ]] || exit 1
    printf '%s\n' "\$unit_exe"
    exit 0
fi
exec "$real_readlink" "\$@"
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
[[ ! -e /proc/$$/fd/9 ]]
load_state() {
    unit_active=inactive unit_sub=dead unit_invocation= unit_pid=0 unit_exe= unit_inv_seq=0
    if [[ -f "$WS_TEST_UNIT_STATE" ]]; then
        source "$WS_TEST_UNIT_STATE"
    fi
}
save_state() {
    {
        printf 'unit_active=%q\n' "$unit_active"
        printf 'unit_sub=%q\n' "$unit_sub"
        printf 'unit_invocation=%q\n' "$unit_invocation"
        printf 'unit_pid=%q\n' "$unit_pid"
        printf 'unit_exe=%q\n' "$unit_exe"
        printf 'unit_inv_seq=%q\n' "$unit_inv_seq"
    } > "$WS_TEST_UNIT_STATE"
}
case "$1" in
    show)
        [[ ${*: -1} == windshare-network.service ]]
        load_state
        if [[ $* == *"--property=InvocationID --value"* ]]; then
            printf '%s\n' "$unit_invocation"
            exit 0
        fi
        printf 'ActiveState=%s\nSubState=%s\nInvocationID=%s\nMainPID=%s\n' \
            "$unit_active" "$unit_sub" "$unit_invocation" "$unit_pid"
        exit 0
        ;;
    reset-failed)
        [[ $# == 2 && $2 == windshare-network.service ]]
        rm -f -- "$WS_TEST_FAILED"
        echo "reset-failed" >> "$WS_TEST_UNIT_LOG"
        exit 0
        ;;
    stop)
        [[ $# == 2 && $2 == windshare-network.service ]]
        target=$(basename "$(readlink "$WS_TEST_ROOT/current")")
        [[ -x $WS_TEST_ROOT/releases/$target/wsrelay ]]
        echo "stop:$target" >> "$WS_TEST_UNIT_LOG"
        load_state
        unit_active=inactive unit_sub=dead unit_pid=0 unit_exe=
        save_state
        exit 0
        ;;
    restart)
        [[ $# == 2 && $2 == windshare-network.service ]]
        [[ ! -e $WS_TEST_FAILED ]]
        target=$(basename "$(readlink "$WS_TEST_ROOT/current")")
        echo "restart:$target" >> "$WS_TEST_UNIT_LOG"
        echo "$target" >> "$WS_TEST_LOG"
        load_state
        if [[ $target == "${FAIL_RESTART:-}" ]]; then
            unit_active=failed unit_sub=failed unit_pid=0 unit_exe=
            save_state
            exit 1
        fi
        if [[ -z ${STALE_INVOCATION:-} || -z $unit_invocation ]]; then
            unit_inv_seq=$((unit_inv_seq + 1))
            unit_invocation="inv-$unit_inv_seq"
        fi
        if [[ $target == "${UNIT_CRASH_TARGET:-}" ]]; then
            unit_active=activating unit_sub=auto-restart unit_pid=0 unit_exe=
        elif [[ -n ${DRIFT_EXE:-} ]]; then
            unit_active=active unit_sub=running unit_pid=$((1000 + unit_inv_seq)) unit_exe=$DRIFT_EXE
        else
            unit_active=active unit_sub=running unit_pid=$((1000 + unit_inv_seq))
            unit_exe=$(realpath -e "$WS_TEST_ROOT/releases/$target/wsrelay")
        fi
        save_state
        ;;
    *)
        exit 1
        ;;
esac
SH
cat > "$work/bin/curl" <<'SH'
#!/usr/bin/env bash
[[ ${*: -1} == http://127.0.0.1:8484/healthz ]] || exit 2
if [[ -n ${FORCE_HEALTH_CODE:-} ]]; then
    echo "$FORCE_HEALTH_CODE"
    exit 0
fi
unit_active=inactive unit_exe=
if [[ -f "$WS_TEST_UNIT_STATE" ]]; then
    source "$WS_TEST_UNIT_STATE"
fi
if [[ $unit_active != active || -z $unit_exe ]]; then
    echo 000
    exit 0
fi
running=$(basename "$(dirname "$unit_exe")")
if [[ $running == "${FAIL_HEALTH:-}" ]]; then echo 503; else echo 200; fi
SH
cat > "$work/bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat > "$work/bin/rmdir" <<SH
#!/usr/bin/env bash
if [[ -n \${FAIL_RMDIR:-} && \${*: -1} == */\$FAIL_RMDIR ]]; then
    echo "rmdir: simulated failure: \${*: -1}" >&2
    exit 1
fi
exec "$real_rmdir" "\$@"
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

reset_host() {
    rm -rf -- "$WS_TEST_ROOT" "$WS_TEST_UNIT_STATE"
}

# A legacy host without last-healthy bootstraps it from a healthy current release only after preflight.
reset_host
install_node 2
rm -- "$WS_TEST_ROOT/last-healthy"
FAIL_CONFIG=1 fails install_node 3
[[ ! -e $WS_TEST_ROOT/last-healthy && ! -L $WS_TEST_ROOT/last-healthy ]]
FAIL_HEALTH=$(revision 3) fails install_node 3
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && $(tail -2 "$WS_TEST_LOG") == "$(revision 3)"$'\n'"$(revision 2)" ]]

# A legacy host whose current symlink points to an unstarted candidate while systemd still runs the
# previous release must anchor last-healthy on the running release rather than the unverified symlink.
reset_host
install_node 2
rm -- "$WS_TEST_ROOT/last-healthy"
mkdir -p "$WS_TEST_ROOT/releases/$(revision 3)"
cp "$work/wsrelay" "$WS_TEST_ROOT/releases/$(revision 3)/wsrelay"
chmod 0755 "$WS_TEST_ROOT/releases/$(revision 3)/wsrelay"
ln -sfn "$WS_TEST_ROOT/releases/$(revision 3)" "$WS_TEST_ROOT/current"
stderr=$(FAIL_HEALTH=$(revision 3) fails install_node 3 2>&1)
[[ $stderr == *"event=wsrelay_install revision=$(revision 3) note=unverified_current current=$(revision 3) last_healthy=$(revision 2)"* ]]
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && $(tail -2 "$WS_TEST_LOG") == "$(revision 3)"$'\n'"$(revision 2)" ]]

# A legacy host with a stopped unit must not bootstrap last-healthy from a rogue HTTP 200 listener.
reset_host
mkdir -p "$WS_TEST_ROOT/releases/$(revision 1)"
cp "$work/wsrelay" "$WS_TEST_ROOT/releases/$(revision 1)/wsrelay"
chmod 0755 "$WS_TEST_ROOT/releases/$(revision 1)/wsrelay"
ln -sfn "$WS_TEST_ROOT/releases/$(revision 1)" "$WS_TEST_ROOT/current"
: > "$WS_TEST_UNIT_LOG"
stderr=$(FORCE_HEALTH_CODE=200 UNIT_CRASH_TARGET=$(revision 1) fails install_node 1 2>&1)
[[ $stderr == *"note=unverified_current current=$(revision 1) last_healthy=none"* ]]
[[ $stderr == *"note=verify_failed target=$(revision 1) http=200 active=activating sub=auto-restart"* ]]
[[ ! -e $WS_TEST_ROOT/current && ! -L $WS_TEST_ROOT/current && ! -e $WS_TEST_ROOT/last-healthy && ! -L $WS_TEST_ROOT/last-healthy ]]
[[ $(cat "$WS_TEST_UNIT_LOG") == $'reset-failed\nrestart:'"$(revision 1)"$'\nstop:'"$(revision 1)"$'\nreset-failed' ]]

# HTTP 200 from an external listener must not mark a crashed unit or drifted ExecStart healthy,
# nor may same-revision reinstall pass without a fresh systemd InvocationID.
reset_host
install_node 2
stderr=$(FORCE_HEALTH_CODE=200 UNIT_CRASH_TARGET=$(revision 3) fails install_node 3 2>&1)
[[ $stderr == *"note=verify_failed target=$(revision 3) http=200 active=activating sub=auto-restart"* ]]
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && ! -d $WS_TEST_ROOT/releases/$(revision 3) ]]
stderr=$(FORCE_HEALTH_CODE=200 DRIFT_EXE=$(realpath -e "$WS_TEST_ROOT/releases/$(revision 2)/wsrelay") fails install_node 3 2>&1)
[[ $stderr == *"note=verify_failed target=$(revision 3) http=200 active=active sub=running"* ]]
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" && ! -d $WS_TEST_ROOT/releases/$(revision 3) ]]
stderr=$(STALE_INVOCATION=1 fails install_node 2 2>&1)
[[ $stderr == *"note=verify_failed target=$(revision 2) http=200 active=active sub=running invocation_changed=false"* ]]
[[ $(selected) == "$(revision 2)" && $(last_healthy) == "$(revision 2)" ]]

# An interrupted first install has no last-healthy and unhealthy current; retrying and failing must stop and clear current.
reset_host
mkdir -p "$WS_TEST_ROOT/releases/$(revision 1)"
cp "$work/wsrelay" "$WS_TEST_ROOT/releases/$(revision 1)/wsrelay"
chmod 0755 "$WS_TEST_ROOT/releases/$(revision 1)/wsrelay"
ln -sfn "$WS_TEST_ROOT/releases/$(revision 1)" "$WS_TEST_ROOT/current"
: > "$WS_TEST_UNIT_LOG"
FAIL_HEALTH=$(revision 1) fails install_node 1
[[ ! -e $WS_TEST_ROOT/current && ! -L $WS_TEST_ROOT/current && ! -e $WS_TEST_ROOT/last-healthy && ! -L $WS_TEST_ROOT/last-healthy ]]
[[ $(cat "$WS_TEST_UNIT_LOG") == $'reset-failed\nrestart:'"$(revision 1)"$'\nstop:'"$(revision 1)"$'\nreset-failed' ]]

# Retention cleanup failures must emit a warning without misreporting a committed upgrade as failed,
# leave unexpected directories intact (without stripping wsrelay or bumping mtime), keep pruning clean
# older releases, and never let broken directories crowd out genuine rollbackable releases.
reset_host
install_node 1
install_node 2
install_node 3
mkdir -p "$WS_TEST_ROOT/releases/$(revision 10)"
cp "$work/wsrelay" "$WS_TEST_ROOT/releases/$(revision 10)/wsrelay"
chmod 0755 "$WS_TEST_ROOT/releases/$(revision 10)/wsrelay"
echo "operator note" > "$WS_TEST_ROOT/releases/$(revision 1)/note.txt"
touch -t 202001010000 "$WS_TEST_ROOT/releases/$(revision 10)"
touch -t 202001020000 "$WS_TEST_ROOT/releases/$(revision 1)"
touch -t 202001030000 "$WS_TEST_ROOT/releases/$(revision 2)"
touch -t 202001040000 "$WS_TEST_ROOT/releases/$(revision 3)"
stderr=$(install_node 4 2>&1)
[[ $(selected) == "$(revision 4)" && $(last_healthy) == "$(revision 4)" ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 4) note=prune_warning target=$(revision 1) reason=unexpected_entries"* ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 4) result=healthy"* ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 1)/wsrelay && -f $WS_TEST_ROOT/releases/$(revision 1)/note.txt ]]
[[ ! -d $WS_TEST_ROOT/releases/$(revision 10) ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 2)/wsrelay && -x $WS_TEST_ROOT/releases/$(revision 3)/wsrelay ]]

# A subsequent upgrade must keep the 3 newest rollbackable releases (5, 4, 3), prune clean 2,
# and keep skipped 1 untouched rather than letting it steal a retention slot via bumped mtime.
stderr=$(install_node 5 2>&1)
[[ $(selected) == "$(revision 5)" && $(last_healthy) == "$(revision 5)" ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 5) note=prune_warning target=$(revision 1) reason=unexpected_entries"* ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 5) result=healthy"* ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 3)/wsrelay && -x $WS_TEST_ROOT/releases/$(revision 4)/wsrelay && -x $WS_TEST_ROOT/releases/$(revision 5)/wsrelay ]]
[[ ! -d $WS_TEST_ROOT/releases/$(revision 2) ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 1)/wsrelay && -f $WS_TEST_ROOT/releases/$(revision 1)/note.txt ]]

# A pre-existing binary-less 40-hex directory with a newer mtime must not occupy a retention slot.
mkdir -p "$WS_TEST_ROOT/releases/$(revision 9)"
echo "stray" > "$WS_TEST_ROOT/releases/$(revision 9)/stray.txt"
touch -t 202001030000 "$WS_TEST_ROOT/releases/$(revision 3)"
touch -t 202001040000 "$WS_TEST_ROOT/releases/$(revision 4)"
touch -t 202001050000 "$WS_TEST_ROOT/releases/$(revision 5)"
touch -t 202501010000 "$WS_TEST_ROOT/releases/$(revision 9)"
stderr=$(install_node 6 2>&1)
[[ $(selected) == "$(revision 6)" && $(last_healthy) == "$(revision 6)" ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 6) note=prune_warning target=$(revision 9) reason=unexpected_entries"* ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 6) result=healthy"* ]]
[[ -x $WS_TEST_ROOT/releases/$(revision 4)/wsrelay && -x $WS_TEST_ROOT/releases/$(revision 5)/wsrelay && -x $WS_TEST_ROOT/releases/$(revision 6)/wsrelay ]]
[[ ! -d $WS_TEST_ROOT/releases/$(revision 3) ]]

# An OS-level rmdir failure on a clean stale release also logs a prune warning without failing the upgrade.
stderr=$(FAIL_RMDIR=$(revision 4) install_node 7 2>&1)
[[ $(selected) == "$(revision 7)" && $(last_healthy) == "$(revision 7)" ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 7) note=prune_warning target=$(revision 4) reason=remove_failed"* ]]
[[ $stderr == *"event=wsrelay_install revision=$(revision 7) result=healthy"* ]]
echo "wsrelay install, preflight, rollback and retention passed"
