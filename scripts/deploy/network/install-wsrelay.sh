#!/usr/bin/env bash
set -euo pipefail
readonly root=/opt/windshare-network
readonly config=/etc/windshare-network/node.json
readonly unit=windshare-network.service
readonly account=windshare-network
readonly health_url=http://127.0.0.1:8484/healthz
readonly health_attempts=10
readonly retain_releases=3
readonly busy=75
readonly stage_prefix=.incoming
readonly switch_prefix=.switch
readonly temp_suffix_template=XXXXXXXX
readonly temp_suffix_pattern='[A-Za-z0-9_-]{8}'

die() { echo "wsrelay installation: $*" >&2; exit 1; }
[[ $(id -u) == 0 ]] || die "root required"
[[ $# == 2 && $1 =~ ^[0-9a-f]{40}$ && $2 =~ ^[0-9a-f]{64}$ ]] || die "expected git revision and SHA-256, with the binary on stdin"
readonly revision=$1 digest=$2
log() { printf 'event=wsrelay_install revision=%s result=%s\n' "$revision" "$1" >&2; }

install -d -m 0755 "$root/releases"
releases=$(realpath -e "$root/releases")
readonly release="$releases/$revision" current="$root/current" last_healthy="$root/last-healthy"
exec 9>"$root/.install.lock"
flock -w 2 9 || exit "$busy"

resolve_release_link() {
    local link=$1 label=$2 target
    if [[ -L $link ]]; then
        target=$(realpath -e "$link") || die "broken $label release"
        [[ $(dirname "$target") == "$releases" && ${target##*/} =~ ^[0-9a-f]{40}$ && -x $target/wsrelay ]] || die "unexpected $label release"
        printf '%s\n' "$target"
    elif [[ -e $link ]]; then
        die "$label must be a release symlink"
    fi
}

health_code() {
    curl --silent --max-time 1 --noproxy '*' --output /dev/null --write-out '%{http_code}' "$health_url" || true
}

health_ok() {
    [[ $(health_code) == 200 ]]
}

inspect_running_release() {
    unit_active= unit_sub= unit_invocation= unit_pid= unit_exe= unit_release=
    local require_new=false previous_invocation= status line dir
    if (( $# >= 1 )); then
        require_new=true
        previous_invocation=$1
    fi
    status=$(systemctl show --property=ActiveState --property=SubState --property=InvocationID --property=MainPID "$unit" 9>&- 2>/dev/null) || return 1
    while IFS= read -r line; do
        case "$line" in
            ActiveState=*) unit_active=${line#ActiveState=} ;;
            SubState=*) unit_sub=${line#SubState=} ;;
            InvocationID=*) unit_invocation=${line#InvocationID=} ;;
            MainPID=*) unit_pid=${line#MainPID=} ;;
        esac
    done <<< "$status"
    if [[ $unit_pid =~ ^[1-9][0-9]*$ ]]; then
        unit_exe=$(readlink "/proc/$unit_pid/exe" 2>/dev/null) || unit_exe=
    fi
    [[ $unit_active == active && $unit_sub == running && $unit_pid =~ ^[1-9][0-9]*$ ]] || return 1
    if [[ $require_new == true ]]; then
        [[ -n $unit_invocation && $unit_invocation != "$previous_invocation" ]] || return 1
    fi
    [[ ${unit_exe##*/} == wsrelay && -f $unit_exe && ! -L $unit_exe && -x $unit_exe ]] || return 1
    dir=$(dirname "$unit_exe")
    [[ -d $dir && ! -L $dir && $(dirname "$dir") == "$releases" && ${dir##*/} =~ ^[0-9a-f]{40}$ ]] || return 1
    unit_release=$dir
}

# Separate systemd's active symlink (current) from the rollback anchor (last-healthy)
# so interrupting a switch before health passes cannot overwrite the rollback target.
current_release=$(resolve_release_link "$current" "current")
previous=$(resolve_release_link "$last_healthy" "last-healthy")

stage=$(mktemp -d "$releases/${stage_prefix}.${temp_suffix_template}")
switch=
cleanup() {
    rm -f -- "$stage/wsrelay"
    [[ ! -d $stage ]] || rmdir -- "$stage"
    if [[ -n $switch ]]; then
        rm -f -- "$switch/link"
        rmdir -- "$switch"
    fi
}
trap cleanup EXIT
switch=$(mktemp -d "$root/${switch_prefix}.${temp_suffix_template}")
cat > "$stage/wsrelay"
verify() { printf '%s  %s\n' "$digest" "$1" | sha256sum --check --status; }
verify "$stage/wsrelay" || die "SHA-256 mismatch"
chmod 0755 "$stage" "$stage/wsrelay"
# The service account exercises its actual config and filesystem permissions.
runuser -u "$account" -- "$stage/wsrelay" --check-config "$config" 9>&-
new_release=false
if [[ -e $release || -L $release ]]; then
    [[ -d $release && ! -L $release && -f $release/wsrelay && ! -L $release/wsrelay ]] && verify "$release/wsrelay" || die "revision already contains another binary"
else
    mv -T -- "$stage" "$release"
    new_release=true
fi

set_release_link() {
    ln -sfn -- "$1" "$switch/link"
    mv -Tf -- "$switch/link" "$2"
}

select_release() {
    set_release_link "$1" "$current"
}

record_healthy() {
    set_release_link "$1" "$last_healthy"
}

restart_healthy() {
    local expected=$1 old_invocation http_code= invocation_changed=false
    old_invocation=$(systemctl show --property=InvocationID --value "$unit" 9>&- 2>/dev/null || true)
    # Clear any prior start-limit or failed state before both upgrades and rollbacks.
    systemctl reset-failed "$unit" 9>&- || true
    systemctl restart "$unit" 9>&- || return 1
    for ((attempt=0; attempt<health_attempts; attempt++)); do
        http_code=$(health_code)
        # Verify the systemd unit after /healthz succeeds so a crashed new process
        # cannot ride on an external listener answering HTTP 200 on the same port.
        if [[ $http_code == 200 ]] && inspect_running_release "$old_invocation" && [[ $unit_release == "$expected" ]]; then
            return 0
        fi
        sleep 1
    done
    if [[ $http_code != 200 ]]; then
        inspect_running_release "$old_invocation" || true
    fi
    if [[ -n $unit_invocation && $unit_invocation != "$old_invocation" ]]; then
        invocation_changed=true
    fi
    printf 'event=wsrelay_install revision=%s note=verify_failed target=%s http=%s active=%s sub=%s invocation_changed=%s pid=%s exe=%s\n' \
        "$revision" "${expected##*/}" "${http_code:-000}" "${unit_active:-none}" "${unit_sub:-none}" \
        "$invocation_changed" "${unit_pid:-0}" "${unit_exe:-none}" >&2
    return 1
}

if [[ -z $previous ]]; then
    if inspect_running_release && health_ok; then
        # Persist the verified running release before mutating current so an
        # interruption during the first upgrade on a legacy host keeps a rollback target.
        previous=$unit_release
        record_healthy "$previous"
    fi
fi
if [[ -n $current_release && $current_release != "$previous" ]]; then
    last_healthy_label=none
    [[ -z $previous ]] || last_healthy_label=${previous##*/}
    printf 'event=wsrelay_install revision=%s note=unverified_current current=%s last_healthy=%s\n' \
        "$revision" "${current_release##*/}" "$last_healthy_label" >&2
fi

select_release "$release"
if ! restart_healthy "$release"; then
    log startup_failed
    rollback_error=
    if [[ -z $previous ]]; then
        # Stop the unit before removing the symlink and binary it may still run or restart.
        systemctl stop "$unit" 9>&- || true
        systemctl reset-failed "$unit" 9>&- || true
        rm -- "$current"
    elif [[ $previous != "$release" ]]; then
        select_release "$previous"
        if restart_healthy "$previous"; then
            log "rolled_back_to_${previous##*/}"
        else
            rollback_error="rollback to ${previous##*/} failed"
        fi
    fi
    if [[ $new_release == true ]]; then
        rm -- "$release/wsrelay"
        rmdir -- "$release"
    fi
    [[ -z $rollback_error ]] || die "$rollback_error"
    exit 1
fi

warn_prune() {
    printf 'event=wsrelay_install revision=%s note=prune_warning target=%s reason=%s\n' \
        "$revision" "$1" "$2" >&2
}

remove_managed_dir() {
    local dir=$1 name=$2 child=$3 child_type=$4 success_note=${5:-} extra
    # Check for unexpected entries before unlinking so cleanup neither wipes
    # operator files nor leaves a binary-less directory with a bumped mtime.
    if ! extra=$(find "$dir" -mindepth 1 -maxdepth 1 ! \( -name "$child" -type "$child_type" \) -print -quit); then
        warn_prune "$name" inspect_failed
        return 0
    fi
    if [[ -n $extra ]]; then
        warn_prune "$name" unexpected_entries
        return 0
    fi
    if ! rm -f -- "$dir/$child" || ! rmdir -- "$dir"; then
        warn_prune "$name" remove_failed
        return 0
    fi
    if [[ -n $success_note ]]; then
        printf 'event=wsrelay_install revision=%s note=%s target=%s\n' \
            "$revision" "$success_note" "$name" >&2
    fi
}

prune_releases() {
    # Keep the actual rollback target even when older releases were reinstalled.
    # Cleanup runs best-effort after the new release is live and recorded healthy so
    # housekeeping errors never misreport a committed upgrade as failed.
    touch -- "$release" || warn_prune "$revision" touch_failed
    local listed name dir kept=1
    if [[ -n $previous && $previous != "$release" ]]; then kept=2; fi
    if ! listed=$(find "$releases" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %f\n' | LC_ALL=C sort -nr); then
        warn_prune "releases" list_failed
        return 0
    fi
    while IFS=' ' read -r _ name; do
        [[ $name =~ ^[0-9a-f]{40}$ ]] || continue
        dir="$releases/$name"
        [[ $dir != "$release" && $dir != "$previous" ]] || continue
        if [[ -f $dir/wsrelay && ! -L $dir/wsrelay && -x $dir/wsrelay ]] && (( kept < retain_releases )); then
            kept=$((kept + 1))
            continue
        fi
        remove_managed_dir "$dir" "$name" wsrelay f
    done <<< "$listed"
}

prune_orphan_dirs() {
    local parent=$1 prefix=$2 active=$3 child=$4 child_type=$5
    local listed name suffix suffix_re="^${temp_suffix_pattern}$"
    # Holding the exclusive install lock guarantees every other matching temp dir
    # was orphaned by an ungraceful exit (for example SIGKILL or power loss).
    if ! listed=$(find "$parent" -mindepth 1 -maxdepth 1 -type d -name "${prefix}.*" -printf '%f\n'); then
        warn_prune "$prefix" list_failed
        return 0
    fi
    while IFS= read -r name; do
        [[ $name == "${prefix}."* && $name != "$active" ]] || continue
        suffix=${name#"${prefix}."}
        [[ $suffix =~ $suffix_re ]] || continue
        remove_managed_dir "$parent/$name" "$name" "$child" "$child_type" pruned_orphan
    done <<< "$listed"
}

prune_orphans() {
    prune_orphan_dirs "$releases" "$stage_prefix" "${stage##*/}" wsrelay f
    prune_orphan_dirs "$root" "$switch_prefix" "${switch##*/}" link l
}

record_healthy "$release"
prune_releases
prune_orphans
log healthy
