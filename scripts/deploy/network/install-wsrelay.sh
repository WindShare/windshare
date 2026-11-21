#!/usr/bin/env bash
set -euo pipefail
readonly root=/opt/windshare-network
readonly current="$root/current"
readonly config=/etc/windshare-network/node.json
readonly unit=windshare-network.service
readonly account=windshare-network
readonly busy=75
# Exceed windshare-network.service TimeoutStopSec (6s) so SIGKILL fallback and
# fresh spawn finish before failing, while staying inside network-upgrade's switchBudget (18s).
readonly restart_wait_seconds=10
readonly restart_poll_interval=0.2
readonly restart_max_polls=50
# Retain recent releases and operation records for post-release recovery and manual rollback.
readonly retain_releases=3
readonly retain_operations=20

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
    local release=$1 old_invocation status invocation pid line attempt deadline
    ln -sfn -- "$release" "$root/.switch-$operation"
    mv -Tf -- "$root/.switch-$operation" "$current"
    old_invocation=$(systemctl show --property=InvocationID --value "$unit" 9>&-)
    systemctl restart --no-block "$unit" 9>&-
    # Require a fresh InvocationID so same-binary restarts cannot match a still-retiring MainPID.
    # Bound by both wall-clock deadline (in case systemctl show stalls) and poll count.
    deadline=$((SECONDS + restart_wait_seconds))
    for ((attempt=1;attempt<=restart_max_polls;attempt++)); do
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
        if (( attempt >= restart_max_polls || SECONDS >= deadline )); then
            break
        fi
        sleep "$restart_poll_interval"
    done
    log "restart_timeout old_invocation=${old_invocation:-none} invocation=${invocation:-none} pid=${pid:-0} polls=$attempt"
    die "selected binary did not start"
}
sort_basenames_by_mtime_desc() {
    local dir=$1 pattern=$2
    local entry name
    local -a matched=()
    for entry in "$dir"/*; do
        [[ -d $entry && ! -L $entry ]] || continue
        name=${entry##*/}
        [[ $name =~ $pattern ]] || continue
        matched+=("$entry")
    done
    (( ${#matched[@]} > 0 )) || return 0
    # UTC %y formats as YYYY-MM-DD HH:MM:SS.NNNNNNNNN +0000, sorting newest-first without
    # relying on non-POSIX find -printf or losing sub-second order across rapid runs.
    TZ=UTC stat -c '%y|%n' -- "${matched[@]}" | LC_ALL=C sort -r | while IFS='|' read -r _ path; do
        printf '%s\n' "${path##*/}"
    done
}
prune_releases() {
    local current_release previous_release ordered name path kept=0 pruned=0 errors=0
    # Fail closed on release pruning if current or previous cannot be resolved safely.
    current_release=$(selected 2>/dev/null) || {
        printf '0 1\n'
        return 0
    }
    previous_release=$(cat "$state/previous" 2>/dev/null) || {
        printf '0 1\n'
        return 0
    }
    if [[ -e $previous_release ]]; then
        previous_release=$(realpath -e "$previous_release" 2>/dev/null || printf '%s' "$previous_release")
    fi
    if [[ $(dirname "$current_release") == "$releases" && ${current_release##*/} =~ ^[0-9a-f]{40}$ ]]; then
        kept=$((kept + 1))
    fi
    if [[ $previous_release != "$current_release" && $(dirname "$previous_release") == "$releases" && ${previous_release##*/} =~ ^[0-9a-f]{40}$ ]]; then
        kept=$((kept + 1))
    fi
    ordered=$(sort_basenames_by_mtime_desc "$releases" '^[0-9a-f]{40}$') || {
        printf '0 1\n'
        return 0
    }
    while IFS= read -r name; do
        [[ -n $name ]] || continue
        path="$releases/$name"
        if [[ $path == "$current_release" || $path == "$previous_release" ]]; then
            continue
        fi
        if (( kept < retain_releases )); then
            kept=$((kept + 1))
            continue
        fi
        if rm -rf -- "$path"; then
            pruned=$((pruned + 1))
        else
            errors=$((errors + 1))
        fi
    done <<< "$ordered"
    printf '%s %s\n' "$pruned" "$errors"
}
prune_operations() {
    local ops_dir="$root/operations"
    local ordered name kept=1 pruned=0 errors=0
    # Pin the current operation so post-release -recover-operation can still inspect
    # $state/released and $state/previous if the subsequent authority release fails.
    ordered=$(sort_basenames_by_mtime_desc "$ops_dir" '^[a-zA-Z0-9-]{16,64}$') || {
        printf '0 1\n'
        return 0
    }
    while IFS= read -r name; do
        [[ -n $name && $name != "$operation" ]] || continue
        if (( kept < retain_operations )); then
            kept=$((kept + 1))
            continue
        fi
        if rm -rf -- "$ops_dir/$name"; then
            pruned=$((pruned + 1))
        else
            errors=$((errors + 1))
        fi
    done <<< "$ordered"
    printf '%s %s\n' "$pruned" "$errors"
}
prune_orphan_stages() {
    local entry name pruned=0 errors=0
    # Holding the exclusive operations lock guarantees no live stage owns these dirs.
    for entry in "$releases"/.stage-*; do
        [[ -d $entry && ! -L $entry ]] || continue
        name=${entry##*/}
        [[ $name =~ ^\.stage-[A-Za-z0-9_-]{8}$ ]] || continue
        if rm -rf -- "$entry"; then
            pruned=$((pruned + 1))
        else
            errors=$((errors + 1))
        fi
    done
    printf '%s %s\n' "$pruned" "$errors"
}
prune_retained_state() {
    local pruned_releases=0 release_errors=0 pruned_operations=0 op_errors=0 pruned_stages=0 stage_errors=0 total_errors
    read -r pruned_releases release_errors <<< "$(prune_releases)" || release_errors=1
    read -r pruned_operations op_errors <<< "$(prune_operations)" || op_errors=1
    read -r pruned_stages stage_errors <<< "$(prune_orphan_stages)" || stage_errors=1
    total_errors=$((release_errors + op_errors + stage_errors))
    if (( total_errors > 0 )); then
        log "prune_warning pruned_releases=$pruned_releases pruned_operations=$pruned_operations pruned_stages=$pruned_stages errors=$total_errors"
    else
        log "pruned releases=$pruned_releases operations=$pruned_operations stages=$pruned_stages"
    fi
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
        # Refresh mtime on both new and reused revisions so redeploying an older commit keeps it newest.
        touch -- "$release"
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
        touch -- "$state" || true
        # Pruning runs best-effort only after the local hold is cleared under .lock, so
        # cleanup errors cannot block authority release or touch an active unrecovered hold.
        prune_retained_state || log "prune_warning errors=1"
        ;;
    *) die "unknown action" ;;
esac
log complete
