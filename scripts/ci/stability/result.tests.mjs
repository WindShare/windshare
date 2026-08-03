import assert from 'node:assert/strict'
import { createHmac } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { crc32 } from 'node:zlib'

import {
  MAXIMUM_ARTIFACT_ARCHIVE_BYTES,
  parseStabilityResultArchive,
} from './artifact.mjs'
import {
  STABILITY_EVIDENCE_EPOCH,
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
  runStabilityIntegration,
  stabilityEvidenceDigest,
  writeCanonicalJSON,
} from './result.mjs'

const repositoryRoot = resolve('.')
const operatingSystem = 'windows'
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
})
const startedDocument = `${JSON.stringify(started)}\n`

assert.equal(STABILITY_EVIDENCE_EPOCH, 'windshare.stability-evidence-epoch/v1')
assert.equal(
  STABILITY_STARTED_EVENT_SCHEMA_VERSION,
  'windshare.stability-integration-started/v2',
)
assert.equal(STABILITY_RESULT_SCHEMA_VERSION, 'windshare.stability-result/v4')
assert.equal(started.schema_version, STABILITY_STARTED_EVENT_SCHEMA_VERSION)
assert.equal(started.evidence_epoch, STABILITY_EVIDENCE_EPOCH)
assert.equal('execution_contract_semantic_sha256' in started, false)

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
  productVerdict: passedVerdict,
})
assert.equal(passed.schema_version, STABILITY_RESULT_SCHEMA_VERSION)
assert.equal(passed.evidence_epoch, STABILITY_EVIDENCE_EPOCH)
assert.equal(passed.retry_count, 0)
assert.equal('execution_contract' in passed, false)
assert.deepEqual(parseStabilityResult(JSON.stringify(passed)), passed)

const productFailure = createStabilityResult({
  ...identity,
  invocationId,
  startedEventSha256: stabilityEvidenceDigest(startedDocument),
  productVerdict: createProductVerdictForTermination(17, null),
})
assert.equal(productFailure.product_verdict.outcome, 'failed')
assert.equal(productFailure.product_verdict.failure_class, 'product')
assert.equal(productFailure.product_verdict.exit_code, 17)

const signalFailure = createStabilityResult({
  ...identity,
  invocationId,
  startedEventSha256: stabilityEvidenceDigest(startedDocument),
  productVerdict: createProductVerdictForTermination(null, 'SIGTERM'),
})
assert.equal(signalFailure.product_verdict.outcome, 'failed')
assert.equal(signalFailure.product_verdict.failure_class, 'product')
assert.equal(signalFailure.product_verdict.termination_kind, 'signal')
assert.equal(signalFailure.product_verdict.signal, 'SIGTERM')
assert.throws(() => createProductVerdictForTermination(null, null), /canonical product termination/u)

const reordered = reverseRecordOrder({
  ...passed,
  product_verdict: reverseRecordOrder(passed.product_verdict),
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
    ...mutation,
  }))
}
assert.throws(
  () => parseStabilityStartedEvent(JSON.stringify({
    ...started,
    evidence_epoch: 'windshare.stability-evidence-epoch/older',
  })),
  /evidence epoch is unsupported/u,
)
assert.throws(
  () => parseStabilityResult(JSON.stringify({
    ...passed,
    evidence_epoch: 'windshare.stability-evidence-epoch/older',
  })),
  /evidence epoch is unsupported/u,
)
assert.throws(
  () => parseStabilityResult(JSON.stringify({
    ...passed,
    schema_version: 'windshare.stability-result/v3',
  })),
  /schema version is unsupported/u,
)
assert.throws(
  () => parseStabilityResult(JSON.stringify({ ...passed, execution_contract: {} })),
  /fields are invalid/u,
)

const retry = createStabilityResult({
  ...identity,
  workflowRunAttempt: 3,
  invocationId,
  startedEventSha256: stabilityEvidenceDigest(startedDocument),
  productVerdict: passedVerdict,
})
assert.equal(retry.retry_count, 2)
assert.throws(
  () => parseStabilityResult(JSON.stringify({ ...retry, retry_count: 1 })),
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
}), /disagree/u)

assert.equal(detectRuntimeOperatingSystem({ runnerOS: 'Linux', platform: 'linux' }), 'linux')
assert.equal(detectRuntimeOperatingSystem({ runnerOS: 'Windows', platform: 'win32' }), 'windows')
assert.throws(
  () => detectRuntimeOperatingSystem({ runnerOS: 'Windows', platform: 'linux' }),
  /disagrees/u,
)

const validArchive = evidenceArchive(started, passed)
assert.deepEqual(parseStabilityResultArchive(validArchive), passed)

const mismatchResults = [
  createStabilityResult({
    ...identity,
    workflowRunId: '987654321',
    invocationId,
    startedEventSha256: stabilityEvidenceDigest(startedDocument),
    productVerdict: passedVerdict,
  }),
  createStabilityResult({
    ...identity,
    workflowRunAttempt: 2,
    invocationId,
    startedEventSha256: stabilityEvidenceDigest(startedDocument),
    productVerdict: passedVerdict,
  }),
  createStabilityResult({
    ...identity,
    commitSha: 'b'.repeat(40),
    invocationId,
    startedEventSha256: stabilityEvidenceDigest(startedDocument),
    productVerdict: passedVerdict,
  }),
  createStabilityResult({
    ...identity,
    operatingSystem: 'linux',
    workflowJob: STABILITY_WORKFLOW_JOBS.linux.workflowJob,
    invocationId,
    startedEventSha256: stabilityEvidenceDigest(startedDocument),
    productVerdict: passedVerdict,
  }),
  createStabilityResult({
    ...identity,
    invocationId: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
    startedEventSha256: stabilityEvidenceDigest(startedDocument),
    productVerdict: passedVerdict,
  }),
  createStabilityResult({
    ...identity,
    invocationId,
    startedEventSha256: 'b'.repeat(64),
    productVerdict: passedVerdict,
  }),
]
for (const mismatched of mismatchResults) {
  assert.throws(
    () => parseStabilityResultArchive(evidenceArchive(started, mismatched)),
    /started and finished evidence disagree/u,
  )
}
assert.throws(
  () => parseStabilityResultArchive(zipArchive([
    { name: 'started.json', content: startedDocument },
    { name: 'result.json', content: `${JSON.stringify(passed)}\n` },
    { name: 'result-copy.json', content: `${JSON.stringify(passed)}\n` },
  ])),
  /duplicate structured finished results/u,
)

const root = mkdtempSync(join(tmpdir(), 'windshare-stability-result-'))
try {
  const output = join(root, 'nested', 'result.json')
  writeCanonicalJSON(output, passed)
  assert.equal(readFileSync(output, 'utf8'), `${JSON.stringify(passed)}\n`)
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
  const handshake = spawnStartedPublisher(requestPath, startedOutput, secret)
  assert.equal(handshake.status, 0, handshake.stderr)
  assert.equal(readFileSync(startedOutput, 'utf8'), startedDocument)

  const tamperedRequestPath = join(root, 'tampered-request.json')
  const rejectedOutput = join(root, 'handshake', 'rejected.json')
  writeFileSync(tamperedRequestPath, JSON.stringify({
    schema_version: 'windshare.stability-start-request/v1',
    event: {
      ...started,
      commit_sha: 'b'.repeat(40),
    },
    authentication_tag: authenticationTag,
  }))
  const rejected = spawnStartedPublisher(tamperedRequestPath, rejectedOutput, secret)
  assert.equal(rejected.status, 1)
  assert.match(rejected.stderr, /authentication failed/u)
  assert.equal(existsSync(rejectedOutput), false)

  const runtimeOperatingSystem = detectRuntimeOperatingSystem()
  const runtimeJob = STABILITY_WORKFLOW_JOBS[runtimeOperatingSystem]
  const processOutput = join(root, 'process', 'failed-result.json')
  const processStartedOutput = join(root, 'process', 'failed-started.json')
  const propagatedExit = runStabilityIntegration([
    '--output', processOutput,
    '--started-output', processStartedOutput,
    '--run-id', '24680',
    '--run-attempt', '3',
    '--commit-sha', 'c'.repeat(40),
    '--workflow-job', runtimeJob.workflowJob,
    '--suite', 'integration',
    '--entrypoint', runtimeJob.entrypoint,
  ], {
    spawnIntegration: authenticatedProductProcess(runtimeOperatingSystem, 23, null),
  })
  assert.equal(propagatedExit, 23)
  const failedResult = parseStabilityResult(readFileSync(processOutput, 'utf8'))
  assert.equal(failedResult.product_verdict.exit_code, 23)
  assert.equal(failedResult.retry_count, 2)
  assert.equal(
    failedResult.started_event_sha256,
    stabilityEvidenceDigest(readFileSync(processStartedOutput)),
  )

  const signalOutput = join(root, 'process', 'signal-result.json')
  const signalStartedOutput = join(root, 'process', 'signal-started.json')
  const propagatedSignal = runStabilityIntegration([
    '--output', signalOutput,
    '--started-output', signalStartedOutput,
    '--run-id', '24681',
    '--run-attempt', '1',
    '--commit-sha', 'd'.repeat(40),
    '--workflow-job', runtimeJob.workflowJob,
    '--suite', 'integration',
    '--entrypoint', runtimeJob.entrypoint,
  ], {
    spawnIntegration: authenticatedProductProcess(runtimeOperatingSystem, null, 'SIGTERM'),
  })
  assert.equal(propagatedSignal, 1)
  assert.equal(
    parseStabilityResult(readFileSync(signalOutput, 'utf8')).product_verdict.signal,
    'SIGTERM',
  )
} finally {
  rmSync(root, { recursive: true, force: true })
}

console.log('stability-result tests: PASS')

function spawnStartedPublisher(requestPath, outputPath, secret) {
  return spawnSync(process.execPath, [
    fileURLToPath(new URL('./result.mjs', import.meta.url)),
    'started',
  ], {
    cwd: repositoryRoot,
    encoding: 'utf8',
    env: {
      ...process.env,
      WINDSHARE_STABILITY_START_REQUEST: requestPath,
      WINDSHARE_STABILITY_STARTED_OUTPUT: outputPath,
      WINDSHARE_STABILITY_START_SECRET: secret,
    },
  })
}

function authenticatedProductProcess(expectedOperatingSystem, status, signal) {
  return (actualOperatingSystem, environment) => {
    assert.equal(actualOperatingSystem, expectedOperatingSystem)
    const request = JSON.parse(readFileSync(environment.WINDSHARE_STABILITY_START_REQUEST, 'utf8'))
    const event = parseStabilityStartedEvent(request.event)
    const document = `${JSON.stringify(event)}\n`
    const expectedTag = createHmac(
      'sha256',
      Buffer.from(environment.WINDSHARE_STABILITY_START_SECRET, 'hex'),
    )
      .update(document, 'utf8')
      .digest('hex')
    assert.equal(request.authentication_tag, expectedTag)
    writeCanonicalJSON(environment.WINDSHARE_STABILITY_STARTED_OUTPUT, event)
    return { status, signal }
  }
}

function evidenceArchive(startedEvent, result) {
  return zipArchive([
    { name: 'arbitrary/start.payload', content: `${JSON.stringify(startedEvent)}\n` },
    { name: 'arbitrary/finish.payload', content: `${JSON.stringify(result)}\n` },
  ])
}

function zipArchive(entries) {
  const locals = []
  const centrals = []
  let localOffset = 0
  for (const entry of entries) {
    const nameBytes = Buffer.from(entry.name, 'utf8')
    const data = Buffer.from(entry.content, 'utf8')
    const checksum = crc32(data) >>> 0
    const flags = 1 << 11

    const local = Buffer.alloc(30)
    local.writeUInt32LE(0x04034b50, 0)
    local.writeUInt16LE(20, 4)
    local.writeUInt16LE(flags, 6)
    local.writeUInt32LE(checksum, 14)
    local.writeUInt32LE(data.length, 18)
    local.writeUInt32LE(data.length, 22)
    local.writeUInt16LE(nameBytes.length, 26)

    const central = Buffer.alloc(46)
    central.writeUInt32LE(0x02014b50, 0)
    central.writeUInt16LE(20, 4)
    central.writeUInt16LE(20, 6)
    central.writeUInt16LE(flags, 8)
    central.writeUInt32LE(checksum, 16)
    central.writeUInt32LE(data.length, 20)
    central.writeUInt32LE(data.length, 24)
    central.writeUInt16LE(nameBytes.length, 28)
    central.writeUInt32LE(localOffset, 42)

    const localRecord = Buffer.concat([local, nameBytes, data])
    locals.push(localRecord)
    centrals.push(Buffer.concat([central, nameBytes]))
    localOffset += localRecord.length
  }

  const localBytes = Buffer.concat(locals)
  const centralBytes = Buffer.concat(centrals)
  const end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50, 0)
  end.writeUInt16LE(entries.length, 8)
  end.writeUInt16LE(entries.length, 10)
  end.writeUInt32LE(centralBytes.length, 12)
  end.writeUInt32LE(localBytes.length, 16)
  const archive = Buffer.concat([localBytes, centralBytes, end])
  assert.ok(archive.length <= MAXIMUM_ARTIFACT_ARCHIVE_BYTES)
  return archive
}

function reverseRecordOrder(value) {
  return Object.fromEntries(Object.entries(value).reverse())
}
