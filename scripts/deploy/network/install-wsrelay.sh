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

health_ok() {
    [[ $(curl --silent --max-time 1 --noproxy '*' --output /dev/null --write-out '%{http_code}' "$health_url" || true) == 200 ]]
}

# Separate systemd's active symlink (current) from the rollback anchor (last-healthy)
# so interrupting a switch before health passes cannot overwrite the rollback target.
current_release=$(resolve_release_link "$current" "current")
previous=$(resolve_release_link "$last_healthy" "last-healthy")

stage=$(mktemp -d "$releases/.incoming.XXXXXXXX")
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
switch=$(mktemp -d "$root/.switch.XXXXXXXX")
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
    # Clear any prior start-limit or failed state before both upgrades and rollbacks.
    systemctl reset-failed "$unit" 9>&- || true
    systemctl restart "$unit" 9>&- || return 1
    for ((attempt=0; attempt<health_attempts; attempt++)); do
        if health_ok; then
            return 0
        fi
        sleep 1
    done
    return 1
}

if [[ -z $previous && -n $current_release ]]; then
    if health_ok; then
        # Persist the verified pre-existing release before mutating current so an
        # interruption during the first upgrade on a legacy host keeps a rollback target.
        previous=$current_release
        record_healthy "$previous"
    else
        printf 'event=wsrelay_install revision=%s note=unverified_current current=%s last_healthy=none\n' \
            "$revision" "${current_release##*/}" >&2
    fi
elif [[ -n $previous && -n $current_release && $current_release != "$previous" ]]; then
    printf 'event=wsrelay_install revision=%s note=unverified_current current=%s last_healthy=%s\n' \
        "$revision" "${current_release##*/}" "${previous##*/}" >&2
fi

select_release "$release"
if ! restart_healthy; then
    log startup_failed
    rollback_error=
    if [[ -z $previous ]]; then
        # Stop the unit before removing the symlink and binary it may still run or restart.
        systemctl stop "$unit" 9>&- || true
        systemctl reset-failed "$unit" 9>&- || true
        rm -- "$current"
    elif [[ $previous != "$release" ]]; then
        select_release "$previous"
        if restart_healthy; then
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

prune_releases() {
    # Keep the actual rollback target even when older releases were reinstalled.
    # Cleanup runs best-effort after the new release is live and recorded healthy so
    # housekeeping errors never misreport a committed upgrade as failed.
    touch -- "$release" || warn_prune "$revision" touch_failed
    local listed name dir extra kept=1
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
        # Check for unexpected files before unlinking wsrelay so cleanup neither wipes
        # operator files nor leaves a binary-less directory with a bumped mtime.
        if ! extra=$(find "$dir" -mindepth 1 -maxdepth 1 ! \( -name wsrelay -type f \) -print -quit); then
            warn_prune "$name" inspect_failed
            continue
        fi
        if [[ -n $extra ]]; then
            warn_prune "$name" unexpected_entries
            continue
        fi
        if ! rm -f -- "$dir/wsrelay" || ! rmdir -- "$dir"; then
            warn_prune "$name" remove_failed
        fi
    done <<< "$listed"
}

record_healthy "$release"
prune_releases
log healthy
