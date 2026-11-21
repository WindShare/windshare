#!/usr/bin/env bash
set -euo pipefail

readonly script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
readonly source_script="$script_dir/install.sh"
readonly node_key="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
readonly rev_a="1111111111111111111111111111111111111111"
readonly rev_b="2222222222222222222222222222222222222222"

fail() {
    echo "wsrelay install contract: $*" >&2
    exit 1
}

make_fake_wsrelay() {
    local target=$1
    cat >"$target" <<'EOF'
#!/usr/bin/env bash
if [[ -n ${FAKE_WSRELAY_LOG:-} ]]; then
    printf '%s\n' "$0 $*" >>"$FAKE_WSRELAY_LOG"
fi
case "${1:-}" in
    --help) echo "Usage: wsrelay --enter-maintenance <config> <node_key> <operation> <policy_version> <reconnect_budget_ms>" ;;
    --check-config) exit "${FAKE_WSRELAY_CHECK_CONFIG_EXIT:-0}" ;;
    --check-hold) exit "${FAKE_WSRELAY_CHECK_HOLD_EXIT:-0}" ;;
    --enter-maintenance) exit "${FAKE_WSRELAY_ENTER_EXIT:-0}" ;;
    --release-maintenance) exit "${FAKE_WSRELAY_RELEASE_EXIT:-0}" ;;
    *) exit 1 ;;
esac
EOF
    chmod 0755 "$target"
}

prepare_harness() {
    local base=$1 root_path=$2
    local bin_dir="$base/bin" state_dir="$base/mock" wrapped="$base/install.sh"
    mkdir -p "$bin_dir" "$state_dir/proc"
    touch "$base/node.json"

    # Rewrite only the two hardcoded host paths in a temporary copy so the privileged
    # production helper never accepts environment overrides for its root or config path.
    sed \
        -e "s|^readonly root=/opt/windshare-network\$|readonly root=\"$root_path\"|" \
        -e "s|^readonly config=/etc/windshare-network/node.json\$|readonly config=\"$base/node.json\"|" \
        "$source_script" >"$wrapped"
    grep -Fq "readonly root=\"$root_path\"" "$wrapped" || fail "failed to rewrite root in harness"
    grep -Fq "readonly config=\"$base/node.json\"" "$wrapped" || fail "failed to rewrite config in harness"
    chmod 0755 "$wrapped"

    local real_id real_readlink real_install
    real_id=$(command -v id)
    real_readlink=$(command -v readlink)
    real_install=$(command -v install)

    cat >"$bin_dir/id" <<EOF
#!/usr/bin/env bash
if [[ \${1:-} == -u ]]; then echo "\${FAKE_UID:-0}"; exit 0; fi
exec "$real_id" "\$@"
EOF
    case "$(uname -s)" in
        MSYS*|CYGWIN*|MINGW*)
            # MSYS/Cygwin noacl mounts reject chmod 0700 on directories; keep Linux on real install.
            cat >"$bin_dir/install" <<EOF
#!/usr/bin/env bash
if [[ \${1:-} == -d && \${2:-} == -m && \${3:-} == 0700 ]]; then
    shift 3
    exec mkdir -p -- "\$@"
fi
exec "$real_install" "\$@"
EOF
            ;;
    esac
    cat >"$bin_dir/runuser" <<'EOF'
#!/usr/bin/env bash
if { : >&9; } 2>/dev/null; then
    echo "lock FD 9 leaked into runuser" >&2
    exit 99
fi
[[ ${1:-} == -u && ${3:-} == -- ]] || exit 98
shift 3
exec "$@"
EOF
    cat >"$bin_dir/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    cat >"$bin_dir/readlink" <<EOF
#!/usr/bin/env bash
if [[ \${1:-} =~ ^/proc/([0-9]+)/exe\$ ]]; then
    exe_file="$state_dir/proc/\${BASH_REMATCH[1]}.exe"
    [[ -f "\$exe_file" ]] || exit 1
    cat "\$exe_file"
    exit 0
fi
exec "$real_readlink" "\$@"
EOF
    cat >"$bin_dir/systemctl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if { : >&9; } 2>/dev/null; then
    echo "lock FD 9 leaked into systemctl" >&2
    exit 99
fi
state="$state_dir"
if [[ \${1:-} == restart ]]; then
    echo "restart" >>"\$state/systemctl.log"
    exit 0
fi
if [[ \${1:-} == show ]]; then
    shift
    case "\$*" in
        "--property=InvocationID --value windshare-network.service")
            cat "\$state/current_invocation"
            exit 0
            ;;
        "--property=MainPID --value windshare-network.service")
            cat "\$state/current_pid"
            exit 0
            ;;
        "--property=InvocationID --property=MainPID windshare-network.service")
            count=\$(<"\$state/poll_count")
            count=\$((count + 1))
            printf '%s\n' "\$count" >"\$state/poll_count"
            mapfile -t polls <"\$state/polls"
            idx=\$((count - 1))
            if (( idx >= \${#polls[@]} )); then
                idx=\$((\${#polls[@]} - 1))
            fi
            line=\${polls[\$idx]}
            IFS='|' read -r order inv pid exe <<< "\$line"
            printf '%s\n' "\$inv" >"\$state/current_invocation"
            printf '%s\n' "\$pid" >"\$state/current_pid"
            if [[ -n \$pid && -n \$exe ]]; then
                printf '%s\n' "\$exe" >"\$state/proc/\$pid.exe"
            fi
            if [[ \$order == pid_first ]]; then
                printf 'MainPID=%s\nInvocationID=%s\n' "\$pid" "\$inv"
            else
                printf 'InvocationID=%s\nMainPID=%s\n' "\$inv" "\$pid"
            fi
            exit 0
            ;;
    esac
fi
echo "unexpected systemctl args: \$*" >&2
exit 97
EOF
    chmod 0755 "$bin_dir"/*
    printf '0\n' >"$state_dir/poll_count"
}

run_case_same_binary_waits_for_fresh_invocation() (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT

    local root_path="$tmp/opt/windshare-network"
    prepare_harness "$tmp" "$root_path"
    mkdir -p "$root_path/releases/$rev_a"
    make_fake_wsrelay "$root_path/releases/$rev_a/wsrelay"
    ln -sfn -- "$root_path/releases/$rev_a" "$root_path/current"

    local canonical_a
    canonical_a=$(realpath -e "$root_path/releases/$rev_a")
    local sha_a
    sha_a=$(sha256sum "$canonical_a/wsrelay" | awk '{print $1}')

    printf 'inv-old\n' >"$tmp/mock/current_invocation"
    printf '101\n' >"$tmp/mock/current_pid"
    printf '%s/wsrelay\n' "$canonical_a" >"$tmp/mock/proc/101.exe"
    cat >"$tmp/mock/polls" <<EOF
inv_first|inv-old|101|$canonical_a/wsrelay
pid_first|inv-new|0|
pid_first|inv-new|202|$canonical_a/wsrelay
EOF

    local op="op-same-binary-restart-01"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_a" "$sha_a" <"$canonical_a/wsrelay"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" switch "$op" "$node_key"

    [[ $(cat "$tmp/mock/poll_count") == 3 ]] || fail "same-binary switch matched retiring PID early: polls=$(cat "$tmp/mock/poll_count")"
    printf 'PASS: same-binary restart waits for fresh InvocationID\n'
)

run_case_rollback_fails_when_invocation_never_advances() (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT

    local root_path="$tmp/opt/windshare-network"
    prepare_harness "$tmp" "$root_path"
    mkdir -p "$root_path/releases/$rev_a"
    make_fake_wsrelay "$root_path/releases/$rev_a/wsrelay"
    ln -sfn -- "$root_path/releases/$rev_a" "$root_path/current"
    make_fake_wsrelay "$tmp/candidate-bin"
    printf '# candidate-b\n' >>"$tmp/candidate-bin"

    local canonical_a sha_b
    canonical_a=$(realpath -e "$root_path/releases/$rev_a")
    sha_b=$(sha256sum "$tmp/candidate-bin" | awk '{print $1}')

    printf 'inv-retiring\n' >"$tmp/mock/current_invocation"
    printf '101\n' >"$tmp/mock/current_pid"
    printf '%s/wsrelay\n' "$canonical_a" >"$tmp/mock/proc/101.exe"
    printf 'inv_first|inv-retiring|101|%s/wsrelay\n' "$canonical_a" >"$tmp/mock/polls"

    local op="op-rollback-retiring-01"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_b" "$sha_b" <"$tmp/candidate-bin"

    local status=0 output
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" rollback "$op" "$node_key" 2>&1) || status=$?
    [[ $status != 0 ]] || fail "rollback unexpectedly succeeded against retiring PID"
    [[ $output == *"selected binary did not start"* ]] || fail "unexpected rollback error: $output"
    [[ $(cat "$tmp/mock/poll_count") == 50 ]] || fail "expected 50 polls before timeout, got $(cat "$tmp/mock/poll_count")"
    printf 'PASS: rollback rejects retiring previous PID when InvocationID stays unchanged\n'
)

run_case_empty_old_invocation_requires_non_empty_new_invocation() (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT

    local root_path="$tmp/opt/windshare-network"
    prepare_harness "$tmp" "$root_path"
    mkdir -p "$root_path/releases/$rev_a"
    make_fake_wsrelay "$root_path/releases/$rev_a/wsrelay"
    ln -sfn -- "$root_path/releases/$rev_a" "$root_path/current"

    local canonical_a sha_a
    canonical_a=$(realpath -e "$root_path/releases/$rev_a")
    sha_a=$(sha256sum "$canonical_a/wsrelay" | awk '{print $1}')

    : >"$tmp/mock/current_invocation"
    printf '0\n' >"$tmp/mock/current_pid"
    cat >"$tmp/mock/polls" <<EOF
inv_first||101|$canonical_a/wsrelay
inv_first|inv-fresh|102|$canonical_a/wsrelay
EOF

    local op="op-empty-invocation-01"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_a" "$sha_a" <"$canonical_a/wsrelay"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" switch "$op" "$node_key"

    [[ $(cat "$tmp/mock/poll_count") == 2 ]] || fail "expected empty InvocationID to be rejected, polls=$(cat "$tmp/mock/poll_count")"
    printf 'PASS: never-started unit requires non-empty InvocationID\n'
)

run_case_symlinked_root_and_releases_canonicalized() (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT

    mkdir -p "$tmp/data/opt/windshare-network" "$tmp/data/releases-store/$rev_a"
    ln -sfn -- "$tmp/data/opt" "$tmp/opt-link"
    ln -sfn -- "$tmp/data/releases-store" "$tmp/data/opt/windshare-network/releases"

    local root_path="$tmp/opt-link/windshare-network"
    prepare_harness "$tmp" "$root_path"
    make_fake_wsrelay "$tmp/data/releases-store/$rev_a/wsrelay"
    ln -sfn -- "$tmp/data/releases-store/$rev_a" "$root_path/current"
    make_fake_wsrelay "$tmp/candidate-bin"
    printf '# candidate-b\n' >>"$tmp/candidate-bin"

    local canonical_releases canonical_a canonical_b sha_b
    canonical_releases=$(realpath -e "$tmp/data/releases-store")
    canonical_a="$canonical_releases/$rev_a"
    canonical_b="$canonical_releases/$rev_b"
    sha_b=$(sha256sum "$tmp/candidate-bin" | awk '{print $1}')

    printf 'inv-1\n' >"$tmp/mock/current_invocation"
    printf '101\n' >"$tmp/mock/current_pid"
    printf '%s/wsrelay\n' "$canonical_a" >"$tmp/mock/proc/101.exe"
    cat >"$tmp/mock/polls" <<EOF
inv_first|inv-1|101|$canonical_a/wsrelay
inv_first|inv-2|202|$canonical_b/wsrelay
EOF

    local op="op-symlink-releases-01"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_b" "$sha_b" <"$tmp/candidate-bin"

    local state_dir="$root_path/operations/$op"
    [[ $(cat "$state_dir/previous") == "$canonical_a" ]] || fail "previous not canonical: $(cat "$state_dir/previous")"
    [[ $(cat "$state_dir/candidate") == "$canonical_b" ]] || fail "candidate not canonical: $(cat "$state_dir/candidate")"

    PATH="$tmp/bin:$PATH" "$tmp/install.sh" switch "$op" "$node_key"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" inspect "$op" "$node_key"

    printf '0\n' >"$tmp/mock/poll_count"
    cat >"$tmp/mock/polls" <<EOF
pid_first|inv-2|202|$canonical_b/wsrelay
pid_first|inv-3|303|$canonical_a/wsrelay
EOF
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" rollback "$op" "$node_key"
    [[ $(realpath -e "$root_path/current") == "$canonical_a" ]] || fail "rollback did not restore canonical previous release"
    printf 'PASS: symlinked root and releases stay canonical across stage/switch/inspect/rollback\n'
)

run_case_enter_switch_release_lifecycle_and_guards() (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT

    local root_path="$tmp/opt/windshare-network"
    prepare_harness "$tmp" "$root_path"
    mkdir -p "$root_path/releases/$rev_a"
    make_fake_wsrelay "$root_path/releases/$rev_a/wsrelay"
    ln -sfn -- "$root_path/releases/$rev_a" "$root_path/current"
    make_fake_wsrelay "$tmp/candidate-bin"
    printf '# candidate-b\n' >>"$tmp/candidate-bin"

    local canonical_a canonical_b sha_b
    canonical_a=$(realpath -e "$root_path/releases/$rev_a")
    canonical_b="$(realpath -e "$root_path/releases")/$rev_b"
    sha_b=$(sha256sum "$tmp/candidate-bin" | awk '{print $1}')

    printf 'inv-1\n' >"$tmp/mock/current_invocation"
    printf '101\n' >"$tmp/mock/current_pid"
    printf '%s/wsrelay\n' "$canonical_a" >"$tmp/mock/proc/101.exe"
    printf 'inv_first|inv-2|202|%s/wsrelay\n' "$canonical_b" >"$tmp/mock/polls"

    local op="op-full-lifecycle-01"
    local log="$tmp/wsrelay.log"
    FAKE_WSRELAY_LOG="$log" PATH="$tmp/bin:$PATH" \
        "$tmp/install.sh" stage "$op" "$node_key" "$rev_b" "$sha_b" <"$tmp/candidate-bin"

    # Active sessions defer enter with exit code 75 without mutating the current symlink.
    local status=0
    FAKE_WSRELAY_LOG="$log" FAKE_WSRELAY_ENTER_EXIT=75 PATH="$tmp/bin:$PATH" \
        "$tmp/install.sh" enter "$op" "$node_key" 7 58000 || status=$?
    [[ $status == 75 ]] || fail "expected busy exit 75 from enter, got $status"

    FAKE_WSRELAY_LOG="$log" PATH="$tmp/bin:$PATH" "$tmp/install.sh" enter "$op" "$node_key" 7 58000
    grep -Fxq "$canonical_a/wsrelay --enter-maintenance $tmp/node.json $node_key $op 7 58000" "$log" \
        || fail "enter did not forward expected maintenance args: $(cat "$log")"

    FAKE_WSRELAY_LOG="$log" PATH="$tmp/bin:$PATH" "$tmp/install.sh" switch "$op" "$node_key"
    grep -Fxq "$canonical_a/wsrelay --check-hold $tmp/node.json $node_key $op" "$log" \
        || fail "switch did not check durable maintenance hold"

    FAKE_WSRELAY_LOG="$log" PATH="$tmp/bin:$PATH" "$tmp/install.sh" inspect "$op" "$node_key"
    FAKE_WSRELAY_LOG="$log" PATH="$tmp/bin:$PATH" "$tmp/install.sh" release "$op" "$node_key"
    grep -Fxq "$canonical_a/wsrelay --release-maintenance $tmp/node.json $node_key $op" "$log" \
        || fail "release did not clear durable maintenance hold"
    [[ -e "$root_path/operations/$op/released" ]] || fail "release did not record released marker"

    # Re-entering after release must fail even if current is moved back to previous.
    ln -sfn -- "$canonical_a" "$root_path/current"
    local output
    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" enter "$op" "$node_key" 7 58000 2>&1) || status=$?
    [[ $status != 0 && $output == *"operation already released"* ]] || fail "unexpected re-enter result: $status $output"

    # Staging a fresh op and switching current out from under it must block enter.
    local op2="op-moved-current-01"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op2" "$node_key" "$rev_b" "$sha_b" <"$tmp/candidate-bin"
    ln -sfn -- "$canonical_b" "$root_path/current"
    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" enter "$op2" "$node_key" 8 58000 2>&1) || status=$?
    [[ $status != 0 && $output == *"release changed after staging"* ]] || fail "unexpected moved-current enter result: $status $output"
    printf 'PASS: enter/switch/inspect/release lifecycle, args, exit 75, and guards\n'
)

run_case_stage_validation_collisions_and_cleanup() (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT

    local root_path="$tmp/opt/windshare-network"
    prepare_harness "$tmp" "$root_path"
    mkdir -p "$root_path/releases/$rev_a"
    make_fake_wsrelay "$root_path/releases/$rev_a/wsrelay"
    ln -sfn -- "$root_path/releases/$rev_a" "$root_path/current"
    make_fake_wsrelay "$tmp/candidate-b"
    printf '# candidate-b\n' >>"$tmp/candidate-b"
    make_fake_wsrelay "$tmp/candidate-b2"
    printf '# candidate-b-modified\n' >>"$tmp/candidate-b2"

    local rev_c="3333333333333333333333333333333333333333"
    local sha_b sha_b2 bad_sha
    sha_b=$(sha256sum "$tmp/candidate-b" | awk '{print $1}')
    sha_b2=$(sha256sum "$tmp/candidate-b2" | awk '{print $1}')
    bad_sha="0000000000000000000000000000000000000000000000000000000000000000"

    local status=0 output
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "op-bad-checksum-01" "$node_key" "$rev_b" "$bad_sha" <"$tmp/candidate-b" 2>&1) || status=$?
    [[ $status != 0 && $output == *"checksum mismatch"* ]] || fail "expected checksum mismatch: $status $output"
    [[ -z $(find "$root_path/releases" -maxdepth 1 -name '.stage-*') ]] || fail "checksum failure leaked temp stage dir"

    # Candidate lacking reconnect_budget_ms in --help is rejected and cleaned up.
    cat >"$tmp/legacy-bin" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
    --help) echo "Usage: wsrelay --config <node.json>" ;;
    --check-config) exit 0 ;;
    *) exit 1 ;;
esac
EOF
    chmod 0755 "$tmp/legacy-bin"
    local sha_legacy
    sha_legacy=$(sha256sum "$tmp/legacy-bin" | awk '{print $1}')
    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "op-legacy-proto-01" "$node_key" "$rev_b" "$sha_legacy" <"$tmp/legacy-bin" 2>&1) || status=$?
    [[ $status != 0 && $output == *"incompatible maintenance protocol"* ]] || fail "expected protocol mismatch: $status $output"
    [[ -z $(find "$root_path/releases" -maxdepth 1 -name '.stage-*') ]] || fail "protocol failure leaked temp stage dir"

    # Valid stage is idempotent on retry, rejects different bytes for the same revision, and rejects operation reuse.
    local op="op-stage-collision-01"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_b" "$sha_b" <"$tmp/candidate-b"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_b" "$sha_b" <"$tmp/candidate-b"

    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "op-rev-collision-01" "$node_key" "$rev_b" "$sha_b2" <"$tmp/candidate-b2" 2>&1) || status=$?
    [[ $status != 0 && $output == *"revision collision"* ]] || fail "expected revision collision: $status $output"

    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_c" "$sha_b2" <"$tmp/candidate-b2" 2>&1) || status=$?
    [[ $status != 0 && $output == *"operation collision"* ]] || fail "expected operation collision: $status $output"
    printf 'PASS: stage checksum, protocol, idempotency, collisions, and temp cleanup\n'
)

run_case_auth_hold_inspect_and_lock_guards() (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf -- "$tmp"' EXIT

    local root_path="$tmp/opt/windshare-network"
    prepare_harness "$tmp" "$root_path"
    mkdir -p "$root_path/releases/$rev_a" "$root_path/releases/3333333333333333333333333333333333333333"
    make_fake_wsrelay "$root_path/releases/$rev_a/wsrelay"
    make_fake_wsrelay "$root_path/releases/3333333333333333333333333333333333333333/wsrelay"
    ln -sfn -- "$root_path/releases/$rev_a" "$root_path/current"
    make_fake_wsrelay "$tmp/candidate-b"
    printf '# candidate-b\n' >>"$tmp/candidate-b"

    local canonical_a sha_b
    canonical_a=$(realpath -e "$root_path/releases/$rev_a")
    sha_b=$(sha256sum "$tmp/candidate-b" | awk '{print $1}')

    local status=0 output
    output=$(FAKE_UID=1000 PATH="$tmp/bin:$PATH" "$tmp/install.sh" inspect "op-nonroot-check-01" "$node_key" 2>&1) || status=$?
    [[ $status != 0 && $output == *"root required"* ]] || fail "expected root required: $status $output"

    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" inspect "short" "$node_key" 2>&1) || status=$?
    [[ $status != 0 && $output == *"invalid operation or node key"* ]] || fail "expected invalid op error: $status $output"

    local op="op-hold-and-inspect-01"
    PATH="$tmp/bin:$PATH" "$tmp/install.sh" stage "$op" "$node_key" "$rev_b" "$sha_b" <"$tmp/candidate-b"

    # Failing --check-hold must abort before changing current or restarting systemd.
    status=0
    FAKE_WSRELAY_CHECK_HOLD_EXIT=1 PATH="$tmp/bin:$PATH" "$tmp/install.sh" switch "$op" "$node_key" 2>/dev/null || status=$?
    [[ $status != 0 ]] || fail "switch succeeded despite failed hold check"
    [[ $(realpath -e "$root_path/current") == "$canonical_a" ]] || fail "switch mutated current after failed hold check"
    [[ ! -e "$tmp/mock/systemctl.log" ]] || fail "switch invoked systemctl after failed hold check"

    # inspect rejects mismatched running binary and unexpected current revision.
    printf '101\n' >"$tmp/mock/current_pid"
    printf '/some/other/wsrelay\n' >"$tmp/mock/proc/101.exe"
    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" inspect "$op" "$node_key" 2>&1) || status=$?
    [[ $status != 0 && $output == *"selected release is not running"* ]] || fail "expected exe mismatch: $status $output"

    ln -sfn -- "$root_path/releases/3333333333333333333333333333333333333333" "$root_path/current"
    status=0
    output=$(PATH="$tmp/bin:$PATH" "$tmp/install.sh" inspect "$op" "$node_key" 2>&1) || status=$?
    [[ $status != 0 && $output == *"unexpected running revision"* ]] || fail "expected unexpected revision: $status $output"

    # Concurrent invocation while .lock is held exits with busy code 75.
    (
        exec 8>"$root_path/operations/.lock"
        flock 8
        status=0
        PATH="$tmp/bin:$PATH" "$tmp/install.sh" inspect "$op" "$node_key" 8>&- 2>/dev/null || status=$?
        [[ $status == 75 ]] || fail "expected lock contention exit 75, got $status"
    )
    printf 'PASS: auth, check-hold, inspect mismatch, and lock contention guards\n'
)

run_case_same_binary_waits_for_fresh_invocation
run_case_rollback_fails_when_invocation_never_advances
run_case_empty_old_invocation_requires_non_empty_new_invocation
run_case_symlinked_root_and_releases_canonicalized
run_case_enter_switch_release_lifecycle_and_guards
run_case_stage_validation_collisions_and_cleanup
run_case_auth_hold_inspect_and_lock_guards
