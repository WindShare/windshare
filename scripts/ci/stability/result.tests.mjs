import assert from 'node:assert/strict'
import { createHmac } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION,
  STABILITY_RESULT_SCHEMA_VERSION,
  STABILITY_STARTED_EVENT_SCHEMA_VERSION,
  STABILITY_WORKFLOW_JOBS,
  createProductVerdictForTermination,
  createStabilityResult,
  createStabilityStartedEvent,
  detectRuntimeOperatingSystem,
  parseStabilityResult,
  parseStabilityStartedEvent,
  stabilityEvidenceDigest,
  writeCanonicalJSON,
} from './result.mjs'
import {
  createStabilityExecutionContract,
  executionContractEvidenceEqual,
  executionContractsEqual,
  loadCurrentStabilityExecutionContract,
  loadCurrentStabilityExecutionSources,
} from './execution-contract.mjs'

const repositoryRoot = resolve('.')
const STABILITY_HANDSHAKE_VARIABLES = Object.freeze([
  'WINDSHARE_STABILITY_START_REQUEST',
  'WINDSHARE_STABILITY_STARTED_OUTPUT',
  'WINDSHARE_STABILITY_START_SECRET',
])
const operatingSystem = 'windows'
const executionContract = loadCurrentStabilityExecutionContract({
  operatingSystem,
  repositoryRoot,
})
const identity = {
  workflowRunId: '123456789',
  workflowRunAttempt: 1,
  commitSha: 'a'.repeat(40),
  workflowJob: STABILITY_WORKFLOW_JOBS[operatingSystem].workflowJob,
  operatingSystem,
  suite: 'integration',
}
const invocationId = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
const started = createStabilityStartedEvent({
  ...identity,
  invocationId,
  executionContractSemanticSha256: executionContract.semantic_contract_sha256,
})
assert.equal(started.schema_version, STABILITY_STARTED_EVENT_SCHEMA_VERSION)
const startedDocument = `${JSON.stringify(started)}\n`

const passedVerdict = createProductVerdictForTermination(0, null)
assert.deepEqual(passedVerdict, {
  schema_version: STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION,
  outcome: 'passed',
  failure_class: 'none',
  termination_kind: 'exit-code',
  exit_code: 0,
  signal: null,
})
const passed = createStabilityResult({
  ...identity,
  invocationId,
  startedEventSha256: stabilityEvidenceDigest(startedDocument),
  startedExecutionContractSemanticSha256: started.execution_contract_semantic_sha256,
  productVerdict: passedVerdict,
  executionContract,
})
assert.equal(passed.schema_version, STABILITY_RESULT_SCHEMA_VERSION)
assert.equal(passed.retry_count, 0)
assert.deepEqual(parseStabilityResult(JSON.stringify(passed)), passed)

const productFailure = createStabilityResult({
  ...identity,
  invocationId,
  startedEventSha256: stabilityEvidenceDigest(startedDocument),
  productVerdict: createProductVerdictForTermination(17, null),
  executionContract,
})
assert.equal(productFailure.product_verdict.outcome, 'failed')
assert.equal(productFailure.product_verdict.failure_class, 'product')
assert.equal(productFailure.product_verdict.exit_code, 17)

const signalFailure = createStabilityResult({
  ...identity,
  invocationId,
  startedEventSha256: stabilityEvidenceDigest(startedDocument),
  productVerdict: createProductVerdictForTermination(null, 'SIGTERM'),
  executionContract,
})
assert.equal(signalFailure.product_verdict.outcome, 'failed')
assert.equal(signalFailure.product_verdict.failure_class, 'product')
assert.equal(signalFailure.product_verdict.termination_kind, 'signal')
assert.equal(signalFailure.product_verdict.signal, 'SIGTERM')
assert.throws(() => createProductVerdictForTermination(null, null), /canonical product termination/u)

const reordered = reverseRecordOrder({
  ...passed,
  product_verdict: reverseRecordOrder(passed.product_verdict),
  execution_contract: reverseRecordOrder({
    ...passed.execution_contract,
    sources: passed.execution_contract.sources.map(reverseRecordOrder),
  }),
})
assert.deepEqual(
  parseStabilityResult(`\n  ${JSON.stringify(reordered, null, 2)}\n`),
  passed,
  'JSON whitespace and field order are not evidence authorities',
)
assert.deepEqual(
  parseStabilityStartedEvent(JSON.stringify(reverseRecordOrder(started))),
  started,
)

for (const mutation of [
  { workflowRunId: '0' },
  { workflowRunAttempt: 0 },
  { commitSha: 'A'.repeat(40) },
  { workflowJob: 'another-job' },
  { operatingSystem: 'darwin' },
  { suite: 'browser' },
  { invocationId: 'not-a-uuid' },
]) {
  assert.throws(() => createStabilityStartedEvent({
    ...identity,
    invocationId,
    executionContractSemanticSha256: executionContract.semantic_contract_sha256,
    ...mutation,
  }))
}
assert.throws(
  () => parseStabilityResult(JSON.stringify({ ...passed, retry_count: 1 })),
  /retry count disagrees/u,
)
assert.throws(() => parseStabilityResult(JSON.stringify({ ...passed, unexpected: true })))
assert.throws(() => createStabilityResult({
  ...identity,
  invocationId,
  startedEventSha256: stabilityEvidenceDigest(startedDocument),
  productVerdict: {
    ...passedVerdict,
    outcome: 'failed',
  },
  executionContract,
}), /disagree/u)

const linuxSources = loadCurrentStabilityExecutionSources({
  operatingSystem: 'linux',
  repositoryRoot,
})
const linuxContract = loadCurrentStabilityExecutionContract({
  operatingSystem: 'linux',
  repositoryRoot,
})
assert.throws(() => createStabilityExecutionContract({
  operatingSystem: 'linux',
  sources: replaceSource(linuxSources, 'integration-entrypoint', (source) => source.replace(
    'windshare_go_test_json -count=1 ./integration/...',
    'for attempt in 1 2; do\n  windshare_go_test_json -count=1 ./integration/...\ndone',
  )),
}), /internal retry construct/u)
assert.throws(() => createStabilityExecutionContract({
  operatingSystem: 'linux',
  sources: replaceSource(linuxSources, 'workflow', (source) => source.replace(
    '--entrypoint "bash scripts/ci/linux/integration.sh"',
    '--entrypoint "bash scripts/ci/linux/not-integration.sh"',
  )),
}), /must bind|ambiguous/u)
assert.throws(() => createStabilityExecutionContract({
  operatingSystem: 'linux',
  sources: replaceSource(linuxSources, 'integration-entrypoint', (source) => source.replace(
    [
      'if [[ "$stability_evidence_mode" == authenticated ]]; then',
      '  node scripts/ci/stability/result.mjs started',
      '  unset WINDSHARE_STABILITY_START_REQUEST WINDSHARE_STABILITY_STARTED_OUTPUT WINDSHARE_STABILITY_START_SECRET',
      'fi',
      'windshare_go_test_json -count=1 ./integration/...',
    ].join('\n'),
    [
      'windshare_go_test_json -count=1 ./integration/...',
      'if [[ "$stability_evidence_mode" == authenticated ]]; then',
      '  node scripts/ci/stability/result.mjs started',
      '  unset WINDSHARE_STABILITY_START_REQUEST WINDSHARE_STABILITY_STARTED_OUTPUT WINDSHARE_STABILITY_START_SECRET',
      'fi',
    ].join('\n'),
  )),
}), /authenticated mode immediately before Go|ordered incorrectly/u)
assert.throws(() => createStabilityExecutionContract({
  operatingSystem: 'linux',
  sources: replaceSource(linuxSources, 'go-executable-authority', (source) =>
    `${source}\ngo() { :; }\n`),
}), /must not redefine Go/u)
assert.throws(() => createStabilityExecutionContract({
  operatingSystem: 'linux',
  sources: replaceSource(linuxSources, 'go-executable-authority', (source) => source.replace(
    '/proc/$BASHPID/fd/$WINDSHARE_GO_DESCRIPTOR',
    '/proc/$$/fd/$WINDSHARE_GO_DESCRIPTOR',
  )),
}), /missing retained-executable semantics/u)

for (const { role, path } of linuxSources) {
  const comment = path.endsWith('.mjs') ? '// maintenance note' : '# maintenance note'
  const commentOnly = createStabilityExecutionContract({
    operatingSystem: 'linux',
    sources: replaceSource(linuxSources, role, (source) => `${comment}\n${source}\n${comment}\n`),
  })
  assert.notEqual(commentOnly.contract_sha256, linuxContract.contract_sha256)
  assert.equal(executionContractsEqual(commentOnly, linuxContract), true)
  assert.equal(executionContractEvidenceEqual(commentOnly, linuxContract), false)
}

for (const role of linuxSources.map(({ role }) => role)) {
  const changed = createStabilityExecutionContract({
    operatingSystem: 'linux',
    sources: replaceSource(linuxSources, role, behaviorMutation(role)),
  })
  assert.equal(executionContractsEqual(changed, linuxContract), false, `${role} behavior must reset history`)
}

assert.equal(detectRuntimeOperatingSystem({ runnerOS: 'Linux', platform: 'linux' }), 'linux')
assert.equal(detectRuntimeOperatingSystem({ runnerOS: 'Windows', platform: 'win32' }), 'windows')
assert.throws(
  () => detectRuntimeOperatingSystem({ runnerOS: 'Windows', platform: 'linux' }),
  /disagrees/u,
)

const root = mkdtempSync(join(tmpdir(), 'windshare-stability-result-'))
try {
  const output = join(root, 'nested', 'result.json')
  writeCanonicalJSON(output, passed)
  assert.deepEqual(parseStabilityResult(readFileSync(output, 'utf8')), passed)
  assert.throws(() => writeCanonicalJSON(output, passed), /refusing to overwrite/u)

  const requestPath = join(root, 'request.json')
  const startedOutput = join(root, 'handshake', 'started.json')
  const secret = 'ab'.repeat(32)
  const authenticationTag = createHmac('sha256', Buffer.from(secret, 'hex'))
    .update(startedDocument, 'utf8')
    .digest('hex')
  writeFileSync(requestPath, JSON.stringify({
    schema_version: 'windshare.stability-start-request/v1',
    event: reverseRecordOrder(started),
    authentication_tag: authenticationTag,
  }))
  const handshake = spawnSync(process.execPath, [
    fileURLToPath(new URL('./result.mjs', import.meta.url)),
    'started',
  ], {
    cwd: repositoryRoot,
    encoding: 'utf8',
    env: {
      ...process.env,
      WINDSHARE_STABILITY_START_REQUEST: requestPath,
      WINDSHARE_STABILITY_STARTED_OUTPUT: startedOutput,
      WINDSHARE_STABILITY_START_SECRET: secret,
    },
  })
  assert.equal(handshake.status, 0, handshake.stderr)
  assert.equal(readFileSync(startedOutput, 'utf8'), startedDocument)

  const rejectedOutput = join(root, 'handshake', 'rejected.json')
  const rejected = spawnSync(process.execPath, [
    fileURLToPath(new URL('./result.mjs', import.meta.url)),
    'started',
  ], {
    cwd: repositoryRoot,
    encoding: 'utf8',
    env: {
      ...process.env,
      WINDSHARE_STABILITY_START_REQUEST: requestPath,
      WINDSHARE_STABILITY_STARTED_OUTPUT: rejectedOutput,
      WINDSHARE_STABILITY_START_SECRET: 'cd'.repeat(32),
    },
  })
  assert.equal(rejected.status, 1)
  assert.match(rejected.stderr, /authentication failed/u)

  testIntegrationEntrypointModes(root)
} finally {
  rmSync(root, { recursive: true, force: true })
}

console.log('stability-result tests: PASS')

function testIntegrationEntrypointModes(root) {
  const fixtureRoot = join(root, 'integration-entrypoint')
  const ciRoot = join(fixtureRoot, 'scripts', 'ci')
  const probePath = join(fixtureRoot, 'go-invocation.txt')
  let command
  let commandArguments

  if (process.platform === 'win32') {
    const platformRoot = join(ciRoot, 'windows')
    mkdirSync(join(ciRoot, 'goauthority'), { recursive: true })
    mkdirSync(platformRoot, { recursive: true })
    const entrypoint = join(platformRoot, 'integration.ps1')
    writeFileSync(entrypoint, readFileSync(resolve('scripts/ci/windows/integration.ps1')))
    writeFileSync(join(ciRoot, 'goauthority', 'authority.psm1'), [
      'function Enter-WindShareGoAuthority { return [pscustomobject]@{ Active = $true } }',
      'function Invoke-WindShareGoTestJSON {',
      '    [IO.File]::WriteAllText($env:WINDSHARE_INTEGRATION_ENTRY_PROBE, ($args -join " "))',
      '    $global:LASTEXITCODE = 0',
      '}',
      "Export-ModuleMember -Function @('Enter-WindShareGoAuthority', 'Invoke-WindShareGoTestJSON')",
      '',
    ].join('\n'))
    writeFileSync(join(ciRoot, 'test-run-id.psm1'), [
      'function Invoke-WithWindShareTestRunID {',
      '    param([Parameter(Mandatory)][string]$Suite, [Parameter(Mandatory)][scriptblock]$Body)',
      "    if ($Suite -cne 'integration') { throw 'unexpected integration probe suite' }",
      '    $previous = $env:WINDSHARE_TEST_RUN_ID',
      '    try {',
      "        $env:WINDSHARE_TEST_RUN_ID = 'integration-probe'",
      "        & $Body 'integration-probe'",
      '    } finally {',
      '        $env:WINDSHARE_TEST_RUN_ID = $previous',
      '    }',
      '}',
      "Export-ModuleMember -Function 'Invoke-WithWindShareTestRunID'",
      '',
    ].join('\n'))
    command = 'pwsh.exe'
    commandArguments = ['-NoLogo', '-NoProfile', '-NonInteractive', '-File', entrypoint]
  } else {
    mkdirSync(join(ciRoot, 'linux'), { recursive: true })
    mkdirSync(join(ciRoot, 'goauthority'), { recursive: true })
    const entrypoint = join(ciRoot, 'linux', 'integration.sh')
    writeFileSync(entrypoint, readFileSync(resolve('scripts/ci/linux/integration.sh')))
    writeFileSync(join(ciRoot, 'goauthority', 'authority.sh'), [
      'windshare_enter_go_authority() { :; }',
      'windshare_go_test_json() {',
      '  printf "%s" "$*" >"$WINDSHARE_INTEGRATION_ENTRY_PROBE"',
      '}',
      '',
    ].join('\n'))
    writeFileSync(join(ciRoot, 'test-run-id.sh'), [
      'new_windshare_test_run_id() {',
      "  printf '%s\\n' 'integration-probe'",
      '}',
      '',
    ].join('\n'))
    command = 'bash'
    commandArguments = [entrypoint]
  }

  const ordinary = spawnSync(command, commandArguments, {
    cwd: repositoryRoot,
    encoding: 'utf8',
    env: integrationEntrypointEnvironment(probePath),
  })
  assert.equal(ordinary.status, 0, spawnDiagnostic(ordinary))
  assert.match(ordinary.stdout, /stability_evidence=ordinary/u)
  assert.equal(readFileSync(probePath, 'utf8'), '-count=1 ./integration/...')

  const partialStates = [
    { WINDSHARE_STABILITY_START_REQUEST: 'request' },
    { WINDSHARE_STABILITY_STARTED_OUTPUT: 'output' },
    { WINDSHARE_STABILITY_START_SECRET: '' },
    {
      WINDSHARE_STABILITY_START_REQUEST: 'request',
      WINDSHARE_STABILITY_STARTED_OUTPUT: 'output',
    },
    {
      WINDSHARE_STABILITY_START_REQUEST: 'request',
      WINDSHARE_STABILITY_START_SECRET: 'secret',
    },
    {
      WINDSHARE_STABILITY_STARTED_OUTPUT: 'output',
      WINDSHARE_STABILITY_START_SECRET: 'secret',
    },
  ]
  for (const partialState of partialStates) {
    const expectedPresenceCount = Object.keys(partialState).length
    rmSync(probePath, { force: true })
    const partial = spawnSync(command, commandArguments, {
      cwd: repositoryRoot,
      encoding: 'utf8',
      env: integrationEntrypointEnvironment(probePath, partialState),
    })
    assert.notEqual(partial.status, 0, spawnDiagnostic(partial))
    assert.equal(existsSync(probePath), false)
    assert.match(
      `${partial.stdout}\n${partial.stderr}`,
      new RegExp(
        `stability handshake is partial \\(present=${expectedPresenceCount} required=3\\)`,
        'u',
      ),
    )
  }
}

function integrationEntrypointEnvironment(probePath, overrides = {}) {
  const environment = { ...process.env }
  for (const name of STABILITY_HANDSHAKE_VARIABLES) delete environment[name]
  return {
    ...environment,
    WINDSHARE_INTEGRATION_ENTRY_PROBE: probePath,
    NODE_OPTIONS: '--windshare-integration-entrypoint-probe-must-not-run',
    ...overrides,
  }
}

function spawnDiagnostic(result) {
  return [
    result.error?.message,
    result.stdout,
    result.stderr,
  ].filter((value) => typeof value === 'string' && value !== '').join('\n')
}

function replaceSource(sources, role, replace) {
  return sources.map((source) => source.role === role
    ? { role: source.role, path: source.path, source: replace(source.source.toString('utf8')) }
    : source)
}

function behaviorMutation(role) {
  if (role === 'workflow') {
    return (source) => source.replace('timeout-minutes: 15', 'timeout-minutes: 16')
  }
  return (source) => {
    const changed = source.replace(/"revision":(\d+)/u, (_, revision) =>
      `"revision":${Number(revision) + 1}`)
    assert.notEqual(changed, source, `${role} semantic manifest must be present`)
    return changed
  }
}

function reverseRecordOrder(value) {
  return Object.fromEntries(Object.entries(value).reverse())
}
