#!/usr/bin/env bash
set -euo pipefail
readonly root=/opt/windshare-network
readonly current="$root/current"
readonly config=/etc/windshare-network/node.json
readonly unit=windshare-network.service
readonly account=windshare-network
readonly busy=75

die() { echo "wsrelay installation: $*" >&2; exit 1; }
[[ $(id -u) == 0 ]] || die "root required"
[[ $# -ge 3 ]] || die "expected action, operation ID and node key"
action=$1 operation=$2 node=$3
shift 3
[[ $operation =~ ^[a-zA-Z0-9-]{16,64}$ && $node =~ ^[a-zA-Z0-9_-]{43}$ ]] || die "invalid operation or node key"
readonly state="$root/operations/$operation"
# Canonicalize once so candidate/previous paths match /proc/$pid/exe and selected() across symlinked roots.
install -d -m 0755 "$root/releases"
releases=$(realpath -e "$root/releases")
readonly releases
install -d -m 0700 "$root/operations"
exec 9>"$root/operations/.lock"
flock -w 2 9 || exit "$busy"
log() { printf 'wsrelay operation_id=%s node_id=%s action=%s %s\n' "$operation" "$node" "$action" "$*" >&2; }
selected() {
    local path
    path=$(realpath -e "$current")
    [[ $(dirname "$path") == "$releases" && -x "$path/wsrelay" ]] || die "unexpected current release"
    printf '%s\n' "$path"
}
as_service() {
    # Close the root-owned operations lock FD before crossing the privilege boundary.
    runuser -u "$account" -- "$@" 9>&-
}
gate() {
    local previous
    previous=$(cat "$state/previous")
    as_service "$previous/wsrelay" "$1" "$config" "$node" "$operation" "${@:2}"
}
restart() {
    local release=$1 old_invocation status invocation pid line attempt
    ln -sfn -- "$release" "$root/.switch-$operation"
    mv -Tf -- "$root/.switch-$operation" "$current"
    old_invocation=$(systemctl show --property=InvocationID --value "$unit" 9>&-)
    systemctl restart --no-block "$unit" 9>&-
    # Require a fresh InvocationID so same-binary restarts cannot match a still-retiring MainPID.
    for ((attempt=0;attempt<50;attempt++)); do
        status=$(systemctl show --property=InvocationID --property=MainPID "$unit" 9>&-)
        invocation= pid=
        while IFS= read -r line; do
            case "$line" in
                InvocationID=*) invocation=${line#InvocationID=} ;;
                MainPID=*) pid=${line#MainPID=} ;;
            esac
        done <<< "$status"
        if [[ -n $invocation && $invocation != "$old_invocation" && $pid =~ ^[1-9][0-9]*$ && $(readlink "/proc/$pid/exe") == "$release/wsrelay" ]]; then
            return 0
        fi
        sleep 0.1
    done
    die "selected binary did not start"
}

case "$action" in
    stage)
        [[ $# == 2 && $1 =~ ^[0-9a-f]{40}$ && $2 =~ ^[0-9a-f]{64}$ ]] || die "invalid revision or checksum"
        release="$releases/$1"
        stage=$(mktemp -d "$releases/.stage-XXXXXXXX")
        trap 'rm -f -- "$stage/wsrelay"; rmdir -- "$stage" 2>/dev/null || true' EXIT
        cat >"$stage/wsrelay"
        printf '%s  %s\n' "$2" "$stage/wsrelay" | sha256sum --check --status || die "checksum mismatch"
        chmod 0755 "$stage" "$stage/wsrelay"
        previous=$(selected)
        # Both binaries must bind the same installed identity and support the reconnect fence.
        for binary in "$previous/wsrelay" "$stage/wsrelay"; do
            as_service "$binary" --check-config "$config" "$node"
            as_service "$binary" --help | grep -q -- '<reconnect_budget_ms>' || die "incompatible maintenance protocol"
        done
        if [[ -d $release ]]; then
            printf '%s  %s\n' "$2" "$release/wsrelay" | sha256sum --check --status || die "revision collision"
        else
            mv -T -- "$stage" "$release"
        fi
        install -d -m 0700 "$state"
        if [[ -e $state/previous ]]; then
            [[ $(cat "$state/candidate") == "$release" && $(cat "$state/node") == "$node" ]] || die "operation collision"
        else
            printf '%s\n' "$release" >"$state/candidate"
            printf '%s\n' "$node" >"$state/node"
            printf '%s\n' "$previous" >"$state/previous"
        fi
        ;;
    enter)
        [[ $# == 2 && $1 =~ ^[1-9][0-9]*$ && $2 =~ ^[1-9][0-9]*$ && $(cat "$state/node") == "$node" ]] || die "invalid policy version or host"
        [[ $(selected) == "$(cat "$state/previous")" ]] || die "release changed after staging"
        [[ ! -e $state/released ]] || die "operation already released"
        gate --enter-maintenance "$1" "$2"
        ;;
    recover)
        [[ $# == 0 && $(cat "$state/node") == "$node" ]] || die "operation mismatch"
        previous=$(cat "$state/previous")
        as_service "$previous/wsrelay" --check-config "$config" "$node" >&2
        if gate --check-hold >/dev/null 2>&1; then
            printf 'held\n'
        else
            # Only an already-released or unswitched operation may close without a local hold.
            if [[ ! -e $state/released ]]; then
                release=$(selected)
                [[ $release == "$previous" ]] || die "local hold missing for switched release"
            fi
            gate --release-maintenance >&2
            touch "$state/released"
            printf 'released\n'
        fi
        ;;
    inspect)
        [[ $# == 0 && $(cat "$state/node") == "$node" ]] || die "operation mismatch"
        previous=$(cat "$state/previous")
        as_service "$previous/wsrelay" --check-config "$config" "$node"
        release=$(selected)
        [[ $release == "$previous" || $release == "$(cat "$state/candidate")" ]] || die "unexpected running revision"
        pid=$(systemctl show --property=MainPID --value "$unit" 9>&-)
        [[ $pid =~ ^[1-9][0-9]*$ && $(readlink "/proc/$pid/exe") == "$release/wsrelay" ]] || die "selected release is not running"
        ;;
    switch|rollback)
        [[ $# == 0 && $(cat "$state/node") == "$node" ]] || die "operation mismatch"
        # Recheck durable ownership immediately before either binary switch.
        gate --check-hold
        release=$(cat "$state/previous")
        if [[ $action == switch ]]; then release=$(cat "$state/candidate"); fi
        restart "$release"
        ;;
    release)
        [[ $# == 0 && $(cat "$state/node") == "$node" ]] || die "operation mismatch"
        gate --release-maintenance
        touch "$state/released"
        ;;
    *) die "unknown action" ;;
esac
log complete
