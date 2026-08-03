#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
authority_path="$repository_root/scripts/ci/goauthority/authority.sh"
temporary_root="$(mktemp -d)"
cleanup_temporary_root() {
  rm -rf -- "$temporary_root"
}
trap cleanup_temporary_root EXIT

clean_authority_environment=(
  -u GOFLAGS -u GOWORK -u GOOS -u GOARCH -u GOENV -u GOTOOLCHAIN -u GOROOT
  -u WINDSHARE_GO_EXECUTABLE -u WINDSHARE_GO_AUTHORITY_ACTIVE
  -u WINDSHARE_GO_HOST_OS -u WINDSHARE_GO_HOST_ARCH
)

assert_rejected() {
  local label="$1"
  shift
  if env "${clean_authority_environment[@]}" "$@" bash -c \
    'source "$1"; windshare_enter_go_authority' bash "$authority_path" \
    >"$temporary_root/stdout" 2>"$temporary_root/stderr"; then
    echo "$label did not fail closed" >&2
    exit 1
  fi
}

assert_settled() {
  local label="$1"
  shift
  if ! env "${clean_authority_environment[@]}" "$@" bash -c \
    'source "$1"; windshare_enter_go_authority' bash "$authority_path" \
    >"$temporary_root/stdout" 2>"$temporary_root/stderr"; then
    echo "$label did not settle the Go authority" >&2
    exit 1
  fi
}

for name in GOFLAGS GOWORK GOOS GOARCH GOENV GOTOOLCHAIN GOROOT \
  WINDSHARE_GO_EXECUTABLE WINDSHARE_GO_AUTHORITY_ACTIVE \
  WINDSHARE_GO_HOST_OS WINDSHARE_GO_HOST_ARCH; do
  assert_rejected "ambient $name" "$name="
done

# actions/setup-go exports GOTOOLCHAIN=local on hosted runners; it equals the
# owned default and must settle the authority, while any other value must not.
assert_settled 'ambient GOTOOLCHAIN=local' GOTOOLCHAIN=local
assert_rejected 'ambient GOTOOLCHAIN=auto' GOTOOLCHAIN=auto

persisted_root="$temporary_root/persisted"
mkdir -p -- "$persisted_root/go"
for name in GOFLAGS GOWORK GOOS GOARCH GOENV GOTOOLCHAIN GOROOT; do
  printf '%s=hostile\n' "$name" >"$persisted_root/go/env"
  assert_rejected "persisted $name" XDG_CONFIG_HOME="$persisted_root"
done
printf '%s\n' 'GOTOOLCHAIN=local' >"$persisted_root/go/env"
assert_settled 'persisted GOTOOLCHAIN=local' XDG_CONFIG_HOME="$persisted_root"
printf '%s\n' 'GOTOOLCHAIN=auto' >"$persisted_root/go/env"
assert_rejected 'persisted GOTOOLCHAIN=auto' XDG_CONFIG_HOME="$persisted_root"
rm -- "$persisted_root/go/env"

fake_bin="$temporary_root/fake-bin"
mkdir -- "$fake_bin"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$fake_bin/go"
chmod 0700 "$fake_bin/go"
assert_rejected 'fake PATH Go application' PATH="$fake_bin:$PATH"

for name in GOFLAGS GOWORK GOOS GOARCH GOENV GOTOOLCHAIN GOROOT \
  WINDSHARE_GO_EXECUTABLE WINDSHARE_GO_AUTHORITY_ACTIVE \
  WINDSHARE_GO_HOST_OS WINDSHARE_GO_HOST_ARCH; do
  unset "$name" || true
done
source "$authority_path"
windshare_enter_go_authority
baseline_version="$(windshare_go version)"
if [[ ! "$baseline_version" =~ ^go[[:space:]]version[[:space:]]go ]]; then
  echo 'retained Go application returned an invalid version' >&2
  exit 1
fi

candidate_directory="$(dirname -- "$WINDSHARE_GO_CANDIDATE")"
if [[ -w "$candidate_directory" ]]; then
  backup="$candidate_directory/.windshare-go-authority-${PPID}-$$"
  if [[ -e "$backup" ]]; then
    echo "retained-Go adversary backup already exists: $backup" >&2
    exit 1
  fi
  restore_candidate() {
    rm -f -- "$WINDSHARE_GO_CANDIDATE"
    mv -- "$backup" "$WINDSHARE_GO_CANDIDATE"
  }
  mv -- "$WINDSHARE_GO_CANDIDATE" "$backup"
  trap 'restore_candidate; cleanup_temporary_root' EXIT
  printf '%s\n' '#!/usr/bin/env bash' 'exit 91' >"$WINDSHARE_GO_CANDIDATE"
  chmod 0700 "$WINDSHARE_GO_CANDIDATE"

  [[ "$(windshare_go version)" == "$baseline_version" ]]
  windshare_go_consumer node -e \
    'const {execFileSync}=require("node:child_process"); const p=process.env.WINDSHARE_GO_EXECUTABLE; process.stdout.write(execFileSync(p,["version"],{encoding:"utf8"}))' \
    | grep -Fqx "$baseline_version"

  restore_candidate
  trap cleanup_temporary_root EXIT
else
  echo "retained-Go swap adversary: candidate directory is not mutable by this principal"
fi

visibility_fixture="$temporary_root/visibility-fixture"
visibility_output="$temporary_root/visibility-output.jsonl"
mkdir -- "$visibility_fixture"
printf '%s\n' 'module example.invalid/windshare-visibility' 'go 1.24' >"$visibility_fixture/go.mod"
printf '%s\n' \
  'package visibility' \
  'import ("fmt"; "testing")' \
  'func TestPassingScenario(t *testing.T) {' \
  '  fmt.Println(`{"schema_version":"windshare.test-event/v1","outcome":"succeeded"}`)' \
  '}' >"$visibility_fixture/visibility_test.go"
(
  cd -- "$visibility_fixture"
  windshare_go_test_json -count=1 . >"$visibility_output"
)
node - "$visibility_output" <<'NODE'
const fs = require('node:fs')
const records = fs.readFileSync(process.argv[2], 'utf8').trim().split(/\r?\n/u).map(JSON.parse)
const scenario = records
  .filter((record) => record.Action === 'output' && typeof record.Output === 'string')
  .map((record) => {
    try { return JSON.parse(record.Output) } catch { return undefined }
  })
  .find((event) => event?.schema_version === 'windshare.test-event/v1')
if (scenario?.outcome !== 'succeeded') process.exit(1)
NODE

json_call_log="$temporary_root/json-calls"
json_output="$temporary_root/json-output"
windshare_go() {
  printf '%s\n' "$*" >>"$json_call_log"
  printf '%s\n' '{"Action":"output","Output":"{\"schema_version\":\"windshare.test-event/v1\",\"outcome\":\"succeeded\"}\\n"}'
}
windshare_go_test_json -count=1 ./integration/... >"$json_output"
[[ "$(<"$json_call_log")" == 'test -json -count=1 ./integration/...' ]]
grep -Fq 'windshare.test-event/v1' "$json_output"

for owned_argument in test -json --json -json=false --json=true; do
  if windshare_go_test_json "$owned_argument" ./integration/... >/dev/null 2>&1; then
    echo "Go JSON test wrapper accepted owned argument: $owned_argument" >&2
    exit 1
  fi
done
if windshare_go_test_json >/dev/null 2>&1; then
  echo 'Go JSON test wrapper accepted an empty test selection' >&2
  exit 1
fi
GOFLAGS='-run=hidden'
if windshare_go_test_json ./integration/... >/dev/null 2>&1; then
  echo 'Go JSON test wrapper accepted late GOFLAGS authority' >&2
  exit 1
fi
unset GOFLAGS
[[ "$(wc -l <"$json_call_log" | tr -d '[:space:]')" == 1 ]]

echo 'Go authority Linux tests: PASS'
