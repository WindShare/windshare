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
readonly release="$releases/$revision" current="$root/current"
exec 9>"$root/.install.lock"
flock -w 2 9 || exit "$busy"

previous=
if [[ -L $current ]]; then
    previous=$(realpath -e "$current") || die "broken current release"
    [[ $(dirname "$previous") == "$releases" && ${previous##*/} =~ ^[0-9a-f]{40}$ && -x $previous/wsrelay ]] || die "unexpected current release"
elif [[ -e $current ]]; then
    die "current must be a release symlink"
fi

stage=$(mktemp -d "$releases/.incoming.XXXXXXXX")
switch=
cleanup() {
    rm -f -- "$stage/wsrelay"
    [[ ! -d $stage ]] || rmdir -- "$stage"
    if [[ -n $switch ]]; then
        rm -f -- "$switch/current"
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

select_release() {
    ln -s -- "$1" "$switch/current"
    mv -Tf -- "$switch/current" "$current"
}

restart_healthy() {
    # Clear any prior start-limit or failed state before both upgrades and rollbacks.
    systemctl reset-failed "$unit" 9>&- || true
    systemctl restart "$unit" 9>&- || return 1
    for ((attempt=0; attempt<health_attempts; attempt++)); do
        if [[ $(curl --silent --max-time 1 --noproxy '*' --output /dev/null --write-out '%{http_code}' "$health_url") == 200 ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

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

# Keep the actual rollback target even when older releases were reinstalled.
touch "$release"
kept=1
if [[ -n $previous && $previous != "$release" ]]; then kept=2; fi
while read -r _ name; do
    [[ $name =~ ^[0-9a-f]{40}$ ]] || continue
    dir="$releases/$name"
    [[ $dir != "$release" && $dir != "$previous" ]] || continue
    if (( kept < retain_releases )); then
        kept=$((kept+1))
    else
        rm -f -- "$dir/wsrelay"
        rmdir -- "$dir"
    fi
done < <(find "$releases" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %f\n' | LC_ALL=C sort -nr)
log healthy
