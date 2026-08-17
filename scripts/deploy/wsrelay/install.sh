#!/usr/bin/env bash
set -euo pipefail

readonly SERVICE_NAME="windshare-relay.service"
readonly BINARY_PATH="/usr/local/bin/wsrelay"
readonly ROLLBACK_PATH="/usr/local/bin/wsrelay.previous"
readonly STATE_DIRECTORY="/etc/windshare-relay"
readonly DEPLOYMENT_RECORD="$STATE_DIRECTORY/deployment.env"
readonly HEALTH_URL="http://127.0.0.1:8484/healthz"
readonly HEALTH_ATTEMPTS=20
readonly HEALTH_INTERVAL_SECONDS="0.25"
readonly HEALTH_REQUEST_TIMEOUT_SECONDS=2
readonly DEPLOYMENT_LOCK="/run/lock/windshare-wsrelay-deploy.lock"

artifact=""
expected_sha256=""
revision=""
operation_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
binary_candidate=""
record_candidate=""

usage() {
  echo "usage: $0 --artifact <path> --sha256 <digest> --revision <40-char-sha>" >&2
}

log_milestone() {
  local milestone="$1"
  shift
  # Losing a log consumer must not interrupt a binary switch between its
  # atomic rename and the service restart or rollback decision.
  {
    printf 'wsrelay_deploy operation_id=%s milestone=%s' "$operation_id" "$milestone"
    if (($# > 0)); then
      printf ' %s' "$*"
    fi
    printf '\n'
  } || true
}

fail() {
  log_milestone failed "reason=$1" >&2
  exit 1
}

sha256_file() {
  local digest remainder
  read -r digest remainder < <(sha256sum "$1")
  printf '%s\n' "$digest"
}

wait_for_health() {
  local attempt=1
  while ((attempt <= HEALTH_ATTEMPTS)); do
    if curl -fsS --max-time "$HEALTH_REQUEST_TIMEOUT_SECONDS" "$HEALTH_URL" >/dev/null; then
      return 0
    fi
    sleep "$HEALTH_INTERVAL_SECONDS"
    ((attempt += 1))
  done
  return 1
}

write_deployment_record() {
  local previous_sha256="$1"
  local deployed_at_utc
  deployed_at_utc="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  record_candidate="$STATE_DIRECTORY/.deployment.$operation_id"
  printf 'REVISION=%s\nSHA256=%s\nPREVIOUS_SHA256=%s\nDEPLOYED_AT_UTC=%s\n' \
    "$revision" "$expected_sha256" "$previous_sha256" "$deployed_at_utc" >"$record_candidate" || return 1
  chmod 0644 "$record_candidate" || return 1
  mv -f "$record_candidate" "$DEPLOYMENT_RECORD" || return 1
  record_candidate=""
}

rollback_and_fail() {
  local reason="$1"
  local rollback_candidate="$BINARY_PATH.rollback.$operation_id"
  log_milestone rollback_started "reason=$reason"
  if ! install -o root -g root -m 0755 "$ROLLBACK_PATH" "$rollback_candidate" ||
    ! mv -f "$rollback_candidate" "$BINARY_PATH" ||
    ! systemctl restart "$SERVICE_NAME" ||
    ! wait_for_health; then
    rm -f "$rollback_candidate"
    log_milestone rollback_failed "reason=$reason" >&2
    exit 1
  fi
  log_milestone rollback_complete "reason=$reason"
  exit 1
}

cleanup() {
  if [[ -n "$binary_candidate" ]]; then
    rm -f "$binary_candidate" || true
  fi
  if [[ -n "$record_candidate" ]]; then
    rm -f "$record_candidate" || true
  fi
}
trap cleanup EXIT

while (($# > 0)); do
  case "$1" in
    --artifact)
      (($# >= 2)) || { usage; exit 2; }
      artifact="$2"
      shift 2
      ;;
    --sha256)
      (($# >= 2)) || { usage; exit 2; }
      expected_sha256="${2,,}"
      shift 2
      ;;
    --revision)
      (($# >= 2)) || { usage; exit 2; }
      revision="$2"
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

[[ "$(id -u)" == "0" ]] || fail must_run_as_root
[[ -n "$artifact" && -n "$expected_sha256" && -n "$revision" ]] || { usage; exit 2; }
[[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] || fail invalid_sha256
[[ "$revision" =~ ^[0-9a-f]{40}$ ]] || fail invalid_revision
[[ -f "$artifact" && ! -L "$artifact" ]] || fail invalid_artifact
[[ -x "$BINARY_PATH" ]] || fail current_binary_missing
command -v curl >/dev/null || fail curl_missing
command -v flock >/dev/null || fail flock_missing
command -v sha256sum >/dev/null || fail sha256sum_missing

# A deployment lock protects the single rollback slot from concurrent writers.
exec 9>"$DEPLOYMENT_LOCK"
flock -n 9 || fail deployment_in_progress

actual_sha256="$(sha256_file "$artifact")"
[[ "$actual_sha256" == "$expected_sha256" ]] || fail checksum_mismatch
log_milestone artifact_verified "revision=$revision sha256=$expected_sha256"

install -d -o root -g root -m 0755 "$STATE_DIRECTORY"
current_sha256="$(sha256_file "$BINARY_PATH")"
if [[ "$current_sha256" == "$expected_sha256" ]]; then
  if ! systemctl is-active --quiet "$SERVICE_NAME" || ! wait_for_health; then
    systemctl restart "$SERVICE_NAME" || fail idempotent_restart_failed
    wait_for_health || fail idempotent_health_failed
  fi
  previous_sha256="$current_sha256"
  if [[ -f "$ROLLBACK_PATH" ]]; then
    previous_sha256="$(sha256_file "$ROLLBACK_PATH")"
  fi
  write_deployment_record "$previous_sha256" || fail deployment_record_failed
  log_milestone complete "changed=false revision=$revision sha256=$expected_sha256"
  exit 0
fi

binary_candidate="$BINARY_PATH.next.$operation_id"
install -o root -g root -m 0755 "$artifact" "$binary_candidate"
"$binary_candidate" -h >/dev/null 2>&1 || fail binary_probe_failed
cp --preserve=all "$BINARY_PATH" "$ROLLBACK_PATH"
previous_sha256="$current_sha256"

# Rename keeps the old process on its original inode until systemd performs the
# deliberate restart, so a partial copy can never become executable in place.
mv -f "$binary_candidate" "$BINARY_PATH"
binary_candidate=""
log_milestone binary_switched "previous_sha256=$previous_sha256"

systemctl restart "$SERVICE_NAME" || rollback_and_fail service_restart_failed
wait_for_health || rollback_and_fail health_check_failed
log_milestone health_verified "url=$HEALTH_URL"

write_deployment_record "$previous_sha256" || rollback_and_fail deployment_record_failed
log_milestone complete "changed=true revision=$revision sha256=$expected_sha256"
