import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { dirname, extname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const SOURCE_ROOTS = Object.freeze(['scripts', 'web', 'internal', 'e2e', 'cmd', '.github/workflows'])
const SOURCE_EXTENSIONS = new Set(['.go', '.js', '.mjs', '.ts', '.tsx', '.ps1', '.psm1', '.sh', '.yml', '.yaml'])
const TEST_PATH = /(?:^|[\\/])(?:test|tests)(?:[\\/]|\.)|(?:_test\.go|\.tests?\.[^.]+)$/u
const GO_PROCESS_PATTERN = /exec\.Command(?:Context)?\([^\n,]*,?\s*["']go["']/u
const JAVASCRIPT_PROCESS_PATTERN =
  /(?:spawn|spawnSync|execFile|execFileSync|execFileAsync|execFilePromise)\(\s*["']go["']/u
const SHELL_PROCESS_PATTERN = /^\s*(?:[A-Za-z_][A-Za-z0-9_]*=[^\s]+\s+)*go(?:\s|$)/mu
const POWERSHELL_PROCESS_PATTERN = /^\s*&\s*go(?:\.exe)?(?:\s|$)/mui
const WORKFLOW_PROCESS_PATTERN = /^\s*run:\s*go(?:\s|$)/mu

const violations = []
for (const relativeRoot of SOURCE_ROOTS) {
  for (const path of sourceFiles(resolve(REPOSITORY_ROOT, relativeRoot))) {
    const relativePath = path.slice(REPOSITORY_ROOT.length + 1).replaceAll('\\', '/')
    if (TEST_PATH.test(relativePath)) continue
    const source = readFileSync(path, 'utf8')
    const extension = extname(path)
    const patterns = extension === '.go' ? [GO_PROCESS_PATTERN]
      : extension === '.sh' ? [SHELL_PROCESS_PATTERN]
        : extension === '.ps1' || extension === '.psm1' ? [POWERSHELL_PROCESS_PATTERN]
          : extension === '.yml' || extension === '.yaml' ? [WORKFLOW_PROCESS_PATTERN]
            : [JAVASCRIPT_PROCESS_PATTERN]
    for (const pattern of patterns) {
      if (pattern.test(source)) {
        violations.push(`${relativePath} resolves a literal Go subprocess instead of retained authority`)
        break
      }
    }
  }
}
assert.deepEqual(violations, [])

const makefile = readFileSync(resolve(REPOSITORY_ROOT, 'Makefile'), 'utf8')
assert(!/\$\(shell\s+go(?:\s|\))/u.test(makefile), 'Makefile must not execute Go before platform authority')

const linuxRunID = readFileSync(resolve(REPOSITORY_ROOT, 'scripts/ci/test-run-id.sh'), 'utf8')
const windowsRunID = readFileSync(resolve(REPOSITORY_ROOT, 'scripts/ci/test-run-id.psm1'), 'utf8')
assert.match(linuxRunID, /windshare_assert_go_authority_active/u)
assert.doesNotMatch(linuxRunID, /(?:^|\n)\s*(?:function\s+)?go\s*\(\)/u)
assert.match(windowsRunID, /Assert-WindShareGoAuthorityActive/u)
assert.doesNotMatch(windowsRunID, /(?:Set-Alias|function)\s+go\b/ui)

const repositorySpawnConsumers = Object.freeze([
  'e2e/main_test.go',
  'internal/testprocess/owner.go',
  'internal/perfevidence/buildenv.go',
  'scripts/ci/_corevulnerability/main.go',
  'scripts/ci/browsergate/orchestrator.mjs',
  'scripts/ci/go-v1-forbidden.mjs',
  'web/e2e/fixtures/test-ice-topology-runtime.ts',
  'web/e2e/fixtures/v2-real-stack.ts',
  'web/scripts/browser-evidence/process/process-owner-fixture.ts',
  'web/scripts/browser-network-matrix/cli/build-helpers.mjs',
  'web/test/browser-evidence/native-directory-publisher.test.ts',
  'web/test/browser-evidence/test-process-owner-client.test.ts',
])
for (const relativePath of repositorySpawnConsumers) {
  assert.match(
    readFileSync(resolve(REPOSITORY_ROOT, relativePath), 'utf8'),
    /WINDSHARE_GO_EXECUTABLE/u,
    `${relativePath} must consume the retained Go executable`,
  )
}

for (const [platform, extension, sourceMarker, enterMarker] of [
  ['linux', 'sh', 'goauthority/authority.sh', 'windshare_enter_go_authority'],
  ['windows', 'ps1', 'goauthority/authority.psm1', 'Enter-WindShareGoAuthority'],
]) {
  const directory = resolve(REPOSITORY_ROOT, 'scripts/ci', platform)
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    if (!entry.isFile() || extname(entry.name) !== `.${extension}`) continue
    const source = readFileSync(resolve(directory, entry.name), 'utf8')
    const consumesGo = /(?:windshare_go|Invoke-WindShareGo|browsergate\/main\.mjs|browser-network-matrix-helpers)/u.test(source)
    if (!consumesGo) continue
    const sourceIndex = source.indexOf(sourceMarker)
    const enterIndex = source.indexOf(enterMarker)
    assert(sourceIndex >= 0, `${platform}/${entry.name} must load shared Go authority`)
    assert(enterIndex > sourceIndex, `${platform}/${entry.name} must settle shared Go authority after loading it`)
  }
}

for (const [platform, extension, visibleInvocation, hiddenInvocation] of [
  ['linux', 'sh', 'windshare_go_test_json', /^\s*windshare_go\s+test\b/mu],
  ['windows', 'ps1', 'Invoke-WindShareGoTestJSON', /Invoke-WindShareGo\s+test\b/iu],
]) {
  for (const gate of ['check', 'integration', 'e2e-go', 'race', 'coverage']) {
    const source = readFileSync(resolve(REPOSITORY_ROOT, 'scripts/ci', platform, `${gate}.${extension}`), 'utf8')
    assert.equal(
      source.split(visibleInvocation).length - 1,
      1,
      `${platform}/${gate} must expose one scenario-bearing root sweep through Go JSON`,
    )
    assert.doesNotMatch(source, hiddenInvocation, `${platform}/${gate} must not hide passing scenario output`)
  }
}

for (const path of ['scripts/ci/windows/race.ps1', 'scripts/ci/windows/coverage.ps1']) {
  const source = readFileSync(resolve(REPOSITORY_ROOT, path), 'utf8')
  assert.doesNotMatch(source, /CORE_SUITE_TEST_TIMEOUT/u, `${path} must own its timeout instead of reading ambient state`)
  assert.equal(source.split("$coreSuiteTestTimeout = '30m'").length - 1, 1)
}

console.log('Go authority inventory tests: PASS')

function * sourceFiles(root) {
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name === 'test-results') continue
    const path = resolve(root, entry.name)
    if (entry.isDirectory()) {
      yield * sourceFiles(path)
    } else if (entry.isFile() && SOURCE_EXTENSIONS.has(extname(entry.name))) {
      yield path
    }
  }
}
