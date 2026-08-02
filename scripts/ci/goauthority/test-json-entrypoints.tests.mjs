import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')

const contracts = Object.freeze([
  Object.freeze({
    platform: 'linux',
    extension: 'sh',
    visibleCommand: 'windshare_go_test_json',
    hiddenRootTest: /^\s*windshare_go\s+test\b/mu,
    commands: Object.freeze({
      check: Object.freeze([
        'windshare_go_test_json -short -count=1 ./...',
        'windshare_go -C core test -short -count=1 ./...',
      ]),
      integration: Object.freeze(['windshare_go_test_json -count=1 ./integration/...']),
      'e2e-go': Object.freeze(['windshare_go_test_json -count=1 ./e2e']),
      race: Object.freeze([
        'windshare_go_test_json -race -count=1 ./...',
        'windshare_go -C core test -race -count=1 -timeout="$core_suite_test_timeout" ./...',
      ]),
      coverage: Object.freeze([
        'windshare_go_test_json -count=1 ./... -covermode=atomic -coverprofile="$profile_dir/root.cover.out"',
        'windshare_go run "$GO_TEST_COVERAGE" --config=.testcoverage.yml --profile="$profile_dir/root.cover.out"',
        'windshare_go -C core test -count=1 -timeout="$core_suite_test_timeout" ./... \\\n  -covermode=atomic -coverprofile="$profile_dir/core.cover.out"',
        '(cd core && windshare_go run "$GO_TEST_COVERAGE" --config=.testcoverage.yml --profile="$profile_dir/core.cover.out")',
      ]),
    }),
  }),
  Object.freeze({
    platform: 'windows',
    extension: 'ps1',
    visibleCommand: 'Invoke-WindShareGoTestJSON',
    hiddenRootTest: /Invoke-WindShareGo\s+test\b/iu,
    commands: Object.freeze({
      check: Object.freeze([
        'Invoke-WindShareGoTestJSON -short -count=1 ./...',
        'Invoke-WindShareGo -C core test -short -count=1 ./...',
      ]),
      integration: Object.freeze(['Invoke-WindShareGoTestJSON -count=1 ./integration/...']),
      'e2e-go': Object.freeze(['Invoke-WindShareGoTestJSON -count=1 ./e2e']),
      race: Object.freeze([
        'Invoke-WindShareGoTestJSON -race -count=1 ./...',
        'Invoke-WindShareGo -C core test -race -count=1 "-timeout=$coreSuiteTestTimeout" ./...',
      ]),
      coverage: Object.freeze([
        'Invoke-WindShareGoTestJSON -count=1 ./... -covermode=atomic "-coverprofile=$rootProfile"',
        'Invoke-WindShareGo run $coverageTool --config=.testcoverage.yml "--profile=$rootProfile"',
        'Invoke-WindShareGo -C core test -count=1 "-timeout=$coreSuiteTestTimeout" ./... `\n                -covermode=atomic "-coverprofile=$coreProfile"',
        'Invoke-WindShareGo run $coverageTool --config=.testcoverage.yml "--profile=$coreProfile"',
      ]),
    }),
  }),
])

for (const contract of contracts) {
  for (const [gate, expectedCommands] of Object.entries(contract.commands)) {
    const source = readSource(`scripts/ci/${contract.platform}/${gate}.${contract.extension}`)
    assert.equal(
      occurrences(source, contract.visibleCommand),
      1,
      `${contract.platform}/${gate} must execute its scenario-bearing root sweep once`,
    )
    assert.doesNotMatch(
      source,
      contract.hiddenRootTest,
      `${contract.platform}/${gate} must not hide successful root scenario output`,
    )
    for (const command of expectedCommands) {
      assert.equal(
        occurrences(source, command),
        1,
        `${contract.platform}/${gate} must execute ${command} exactly once`,
      )
    }
  }
}

for (const path of ['scripts/ci/windows/race.ps1', 'scripts/ci/windows/coverage.ps1']) {
  const source = readSource(path)
  assert.equal(occurrences(source, "$coreSuiteTestTimeout = '30m'"), 1, `${path} must own one fixed core timeout`)
  assert.doesNotMatch(source, /CORE_SUITE_TEST_TIMEOUT/u, `${path} must not accept ambient timeout authority`)
}

const linuxAuthority = readSource('scripts/ci/goauthority/authority.sh')
assert.match(linuxAuthority, /windshare_go_test_json\(\) \{[\s\S]*windshare_go test -json "\$@"\n\}/u)
assert.equal(occurrences(linuxAuthority, 'windshare_go test -json "$@"'), 1)

const windowsAuthority = readSource('scripts/ci/goauthority/authority.psm1')
assert.match(windowsAuthority, /function Invoke-WindShareGoTestJSON \{[\s\S]*Invoke-WindShareGo test -json @testArguments\n\}/u)
assert.equal(occurrences(windowsAuthority, 'Invoke-WindShareGo test -json @testArguments'), 1)
assert.match(windowsAuthority, /'Invoke-WindShareGoTestJSON'/u)

console.log('Go JSON test entrypoint visibility/cardinality contracts: PASS')

function readSource(relativePath) {
  return readFileSync(resolve(REPOSITORY_ROOT, relativePath), 'utf8').replaceAll('\r\n', '\n')
}

function occurrences(source, fragment) {
  return source.split(fragment).length - 1
}
