import { createHash, createHmac, randomBytes, randomUUID, timingSafeEqual } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import {
  existsSync,
  linkSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import {
  loadCurrentStabilityExecutionContract,
  parseStabilityExecutionContract,
} from './execution-contract.mjs'

export const STABILITY_STARTED_EVENT_SCHEMA_VERSION =
  'windshare.stability-integration-started/v1'
export const STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION =
  'windshare.stability-product-verdict/v1'
export const STABILITY_RESULT_SCHEMA_VERSION = 'windshare.stability-result/v3'

const STABILITY_START_REQUEST_SCHEMA_VERSION = 'windshare.stability-start-request/v1'

// This reviewed manifest lets the execution contract distinguish behavioral
// runner changes from comments without trusting an unversioned wrapper.
export const STABILITY_RESULT_RUNNER_SEMANTICS = '{"schema_version":"windshare.stability-helper-semantics/v1","operating_system":"cross-platform","role":"evidence-runner","revision":2,"command_plan":["prepare-authenticated-start-request","invoke-canonical-entrypoint-once","require-post-setup-start","publish-product-verdict","propagate-product-exit"]}'

export const STABILITY_WORKFLOW_JOBS = Object.freeze({
  linux: Object.freeze({
    workflowJob: 'linux-integration-stability',
    jobName: 'Native integration stability (Linux)',
    runnerLabel: 'ubuntu-latest',
    entrypoint: 'bash scripts/ci/linux/integration.sh',
  }),
  windows: Object.freeze({
    workflowJob: 'windows-integration-stability',
    jobName: 'Native integration stability (Windows)',
    runnerLabel: 'windows-latest',
    entrypoint: './scripts/ci/windows/integration.ps1',
  }),
})

const OPERATING_SYSTEMS = new Set(Object.keys(STABILITY_WORKFLOW_JOBS))
const SUITES = new Set(['integration'])
const PRODUCT_OUTCOMES = new Set(['passed', 'failed'])
const FAILURE_CLASSES = new Set(['none', 'product'])
const INVOCATION_ID_PATTERN = /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/u
const SHA256_PATTERN = /^[a-f0-9]{64}$/u

const RUNNER_OS_MAPPING = new Map([
  ['Linux', 'linux'],
  ['Windows', 'windows'],
])
const PLATFORM_OS_MAPPING = new Map([
  ['linux', 'linux'],
  ['win32', 'windows'],
])

export function detectRuntimeOperatingSystem({
  runnerOS = process.env.RUNNER_OS,
  platform = process.platform,
} = {}) {
  const platformOS = PLATFORM_OS_MAPPING.get(platform)
  if (platformOS === undefined) throw new Error(`unsupported runtime platform ${String(platform)}`)
  if (runnerOS === undefined || runnerOS === '') return platformOS

  const declaredOS = RUNNER_OS_MAPPING.get(runnerOS)
  if (declaredOS === undefined) throw new Error(`unsupported GitHub runner OS ${runnerOS}`)
  if (declaredOS !== platformOS) {
    throw new Error(`GitHub runner OS ${runnerOS} disagrees with runtime platform ${platform}`)
  }
  return platformOS
}

export function createStabilityStartedEvent(input) {
  const identity = validateEvidenceIdentity(input)
  const invocationID = requireInvocationID(input.invocationId)
  const semanticContractSha256 = requireSHA256(
    input.executionContractSemanticSha256,
    'execution contract semantic digest',
  )
  return Object.freeze({
    schema_version: STABILITY_STARTED_EVENT_SCHEMA_VERSION,
    ...identity,
    invocation_id: invocationID,
    execution_contract_semantic_sha256: semanticContractSha256,
  })
}

export function parseStabilityStartedEvent(encoded) {
  const value = parseJSONObject(encoded, 'stability started event')
  requireExactFields(value, [
    'schema_version',
    'workflow_run_id',
    'workflow_run_attempt',
    'commit_sha',
    'workflow_job',
    'operating_system',
    'suite',
    'invocation_id',
    'execution_contract_semantic_sha256',
  ], 'stability started event')
  if (value.schema_version !== STABILITY_STARTED_EVENT_SCHEMA_VERSION) {
    throw new Error('stability started event schema version is unsupported')
  }
  return createStabilityStartedEvent({
    workflowRunId: value.workflow_run_id,
    workflowRunAttempt: value.workflow_run_attempt,
    commitSha: value.commit_sha,
    workflowJob: value.workflow_job,
    operatingSystem: value.operating_system,
    suite: value.suite,
    invocationId: value.invocation_id,
    executionContractSemanticSha256: value.execution_contract_semantic_sha256,
  })
}

export function createStabilityResult(input) {
  const identity = validateEvidenceIdentity(input)
  const invocationID = requireInvocationID(input.invocationId)
  const startedEventSha256 = requireSHA256(input.startedEventSha256, 'started event digest')
  const productVerdict = createProductVerdict(input.productVerdict)
  const executionContract = parseExecutionContractWithoutFieldOrder(input.executionContract)
  if (executionContract.operating_system !== identity.operating_system) {
    throw new Error('stability result execution contract disagrees with its operating system')
  }
  if (
    input.startedExecutionContractSemanticSha256 !== undefined &&
    input.startedExecutionContractSemanticSha256 !== executionContract.semantic_contract_sha256
  ) {
    throw new Error('stability result execution contract disagrees with its started event')
  }
  return Object.freeze({
    schema_version: STABILITY_RESULT_SCHEMA_VERSION,
    ...identity,
    invocation_id: invocationID,
    started_event_sha256: startedEventSha256,
    product_verdict: productVerdict,
    retry_count: identity.workflow_run_attempt - 1,
    execution_contract: executionContract,
  })
}

export function parseStabilityResult(encoded) {
  const value = parseJSONObject(encoded, 'stability result')
  requireExactFields(value, [
    'schema_version',
    'workflow_run_id',
    'workflow_run_attempt',
    'commit_sha',
    'workflow_job',
    'operating_system',
    'suite',
    'invocation_id',
    'started_event_sha256',
    'product_verdict',
    'retry_count',
    'execution_contract',
  ], 'stability result')
  if (value.schema_version !== STABILITY_RESULT_SCHEMA_VERSION) {
    throw new Error('stability result schema version is unsupported')
  }
  const result = createStabilityResult({
    workflowRunId: value.workflow_run_id,
    workflowRunAttempt: value.workflow_run_attempt,
    commitSha: value.commit_sha,
    workflowJob: value.workflow_job,
    operatingSystem: value.operating_system,
    suite: value.suite,
    invocationId: value.invocation_id,
    startedEventSha256: value.started_event_sha256,
    productVerdict: value.product_verdict,
    executionContract: value.execution_contract,
  })
  if (value.retry_count !== result.retry_count) {
    throw new Error('stability result retry count disagrees with its workflow attempt')
  }
  return result
}

export function stabilityEvidenceDigest(value) {
  const bytes = typeof value === 'string' ? Buffer.from(value, 'utf8') : asBuffer(value)
  return createHash('sha256').update(bytes).digest('hex')
}

export function writeCanonicalJSON(outputPath, value) {
  const destination = resolve(outputPath)
  if (existsSync(destination)) throw new Error(`refusing to overwrite existing result ${destination}`)
  mkdirSync(dirname(destination), { recursive: true, mode: 0o700 })
  const temporary = `${destination}.${process.pid}.${randomUUID()}.tmp`
  try {
    writeFileSync(temporary, `${JSON.stringify(value)}\n`, { encoding: 'utf8', flag: 'wx', mode: 0o600 })
    // Linking a complete same-directory file is both atomic and exclusive on
    // NTFS/ext4, so concurrent publishers cannot replace an authenticated event.
    linkSync(temporary, destination)
  } finally {
    rmSync(temporary, { force: true })
  }
}

function validateEvidenceIdentity(input) {
  const workflowRunId = requireRunId(input.workflowRunId)
  const workflowRunAttempt = requirePositiveInteger(input.workflowRunAttempt, 'workflow run attempt')
  const commitSha = requireCommitSHA(input.commitSha)
  const operatingSystem = requireEnum(input.operatingSystem, OPERATING_SYSTEMS, 'operating system')
  const suite = requireEnum(input.suite, SUITES, 'suite')
  const authority = STABILITY_WORKFLOW_JOBS[operatingSystem]
  if (input.workflowJob !== authority.workflowJob) {
    throw new Error('workflow job identity is invalid for the operating system')
  }
  return Object.freeze({
    workflow_run_id: workflowRunId,
    workflow_run_attempt: workflowRunAttempt,
    commit_sha: commitSha,
    workflow_job: authority.workflowJob,
    operating_system: operatingSystem,
    suite,
  })
}

function createProductVerdict(value) {
  requireExactFields(value, [
    'schema_version',
    'outcome',
    'failure_class',
    'termination_kind',
    'exit_code',
    'signal',
  ], 'stability product verdict')
  if (value.schema_version !== STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION) {
    throw new Error('stability product verdict schema version is unsupported')
  }
  const outcome = requireEnum(value.outcome, PRODUCT_OUTCOMES, 'product outcome')
  const failureClass = requireEnum(value.failure_class, FAILURE_CLASSES, 'failure class')
  if (value.termination_kind === 'exit-code') {
    const exitCode = requireExitCode(value.exit_code)
    if (
      value.signal !== null ||
      (outcome === 'passed' && (failureClass !== 'none' || exitCode !== 0)) ||
      (outcome === 'failed' && (failureClass !== 'product' || exitCode === 0))
    ) {
      throw new Error('stability product verdict fields disagree')
    }
    return Object.freeze({
      schema_version: STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION,
      outcome,
      failure_class: failureClass,
      termination_kind: 'exit-code',
      exit_code: exitCode,
      signal: null,
    })
  }
  if (
    value.termination_kind !== 'signal' ||
    value.exit_code !== null ||
    typeof value.signal !== 'string' ||
    !/^SIG[A-Z0-9]+$/u.test(value.signal) ||
    outcome !== 'failed' ||
    failureClass !== 'product'
  ) {
    throw new Error('stability product verdict fields disagree')
  }
  return Object.freeze({
    schema_version: STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION,
    outcome: 'failed',
    failure_class: 'product',
    termination_kind: 'signal',
    exit_code: null,
    signal: value.signal,
  })
}

export function createProductVerdictForTermination(status, signal) {
  if (Number.isSafeInteger(status)) {
    const exitCode = requireExitCode(status)
    return Object.freeze({
      schema_version: STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION,
      outcome: exitCode === 0 ? 'passed' : 'failed',
      failure_class: exitCode === 0 ? 'none' : 'product',
      termination_kind: 'exit-code',
      exit_code: exitCode,
      signal: null,
    })
  }
  if (typeof signal !== 'string' || !/^SIG[A-Z0-9]+$/u.test(signal)) {
    throw new Error('stability integration ended without a canonical product termination')
  }
  return Object.freeze({
    schema_version: STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION,
    outcome: 'failed',
    failure_class: 'product',
    termination_kind: 'signal',
    exit_code: null,
    signal,
  })
}

function parseExecutionContractWithoutFieldOrder(value) {
  requireExactFields(value, [
    'schema_version',
    'operating_system',
    'workflow_command',
    'integration_command',
    'invocation_count',
    'retry_policy',
    'sources',
    'semantic_contract_sha256',
    'contract_sha256',
  ], 'stability execution contract')
  if (!Array.isArray(value.sources)) {
    throw new Error('stability execution contract source closure is invalid')
  }
  const sources = value.sources.map((source) => {
    requireExactFields(source, [
      'role',
      'path',
      'git_blob_sha1',
      'content_sha256',
      'semantic_sha256',
    ], 'stability execution contract source descriptor')
    return {
      role: source.role,
      path: source.path,
      git_blob_sha1: source.git_blob_sha1,
      content_sha256: source.content_sha256,
      semantic_sha256: source.semantic_sha256,
    }
  })
  return parseStabilityExecutionContract({
    schema_version: value.schema_version,
    operating_system: value.operating_system,
    workflow_command: value.workflow_command,
    integration_command: value.integration_command,
    invocation_count: value.invocation_count,
    retry_policy: value.retry_policy,
    sources,
    semantic_contract_sha256: value.semantic_contract_sha256,
    contract_sha256: value.contract_sha256,
  })
}

function parseJSONObject(encoded, label) {
  let value
  try {
    value = typeof encoded === 'string' ? JSON.parse(encoded) : encoded
  } catch (cause) {
    throw new Error(`${label} is not JSON`, { cause })
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value
}

function requireExactFields(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  const actual = Object.keys(value)
  const expectedSet = new Set(expected)
  if (actual.length !== expected.length || actual.some((key) => !expectedSet.has(key))) {
    throw new Error(`${label} fields are invalid`)
  }
}

function requireRunId(value) {
  if (typeof value !== 'string' || !/^[1-9][0-9]*$/u.test(value)) {
    throw new Error('workflow run ID must be a canonical positive decimal string')
  }
  return value
}

function requirePositiveInteger(value, label) {
  const number = typeof value === 'string' && /^[1-9][0-9]*$/u.test(value)
    ? Number(value)
    : value
  if (!Number.isSafeInteger(number) || number < 1) throw new Error(`${label} must be a positive integer`)
  return number
}

function requireCommitSHA(value) {
  if (typeof value !== 'string' || !/^[a-f0-9]{40}$/u.test(value)) {
    throw new Error('commit SHA must be a canonical SHA-1 object ID')
  }
  return value
}

function requireInvocationID(value) {
  if (typeof value !== 'string' || !INVOCATION_ID_PATTERN.test(value)) {
    throw new Error('stability invocation ID is invalid')
  }
  return value
}

function requireSHA256(value, label) {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) {
    throw new Error(`${label} is invalid`)
  }
  return value
}

function requireExitCode(value) {
  if (!Number.isSafeInteger(value) || value < 0 || value > 255) {
    throw new Error('product exit code must be an integer between 0 and 255')
  }
  return value
}

function requireEnum(value, allowed, label) {
  if (typeof value !== 'string' || !allowed.has(value)) throw new Error(`${label} is invalid`)
  return value
}

function parseOptions(arguments_) {
  const options = new Map()
  for (let index = 0; index < arguments_.length; index += 2) {
    const name = arguments_[index]
    const value = arguments_[index + 1]
    if (typeof name !== 'string' || !name.startsWith('--') || value === undefined) {
      throw new Error('stability result options must be --name value pairs')
    }
    if (options.has(name)) throw new Error(`duplicate stability result option ${name}`)
    options.set(name, value)
  }
  return options
}

function requiredOption(options, name) {
  const value = options.get(name)
  if (value === undefined || value === '') throw new Error(`missing required option ${name}`)
  return value
}

function runIntegration(arguments_) {
  const options = parseOptions(arguments_)
  const allowed = new Set([
    '--output',
    '--started-output',
    '--run-id',
    '--run-attempt',
    '--commit-sha',
    '--workflow-job',
    '--suite',
    '--entrypoint',
  ])
  for (const name of options.keys()) if (!allowed.has(name)) throw new Error(`unsupported option ${name}`)

  const output = resolve(requiredOption(options, '--output'))
  const startedOutput = resolve(requiredOption(options, '--started-output'))
  if (output === startedOutput) throw new Error('started and finished stability outputs must be distinct')
  if (existsSync(output) || existsSync(startedOutput)) {
    throw new Error('refusing to run with an existing stability evidence path')
  }

  const operatingSystem = detectRuntimeOperatingSystem()
  const authority = STABILITY_WORKFLOW_JOBS[operatingSystem]
  const entrypoint = requiredOption(options, '--entrypoint')
  if (entrypoint !== authority.entrypoint) {
    throw new Error('stability integration entrypoint is not canonical for the runtime operating system')
  }
  const executionContract = loadCurrentStabilityExecutionContract({
    operatingSystem,
    repositoryRoot: process.cwd(),
  })
  const identity = {
    workflowRunId: requiredOption(options, '--run-id'),
    workflowRunAttempt: requiredOption(options, '--run-attempt'),
    commitSha: requiredOption(options, '--commit-sha'),
    workflowJob: requiredOption(options, '--workflow-job'),
    operatingSystem,
    suite: requiredOption(options, '--suite'),
  }
  const invocationId = randomUUID()
  const started = createStabilityStartedEvent({
    ...identity,
    invocationId,
    executionContractSemanticSha256: executionContract.semantic_contract_sha256,
  })
  const startedDocument = `${JSON.stringify(started)}\n`
  const requestRoot = mkdtempSync(join(tmpdir(), 'windshare-stability-start-'))
  const requestPath = join(requestRoot, 'request.json')
  const secret = randomBytes(32).toString('hex')
  const request = Object.freeze({
    schema_version: STABILITY_START_REQUEST_SCHEMA_VERSION,
    event: started,
    authentication_tag: authenticateStartedEvent(secret, startedDocument),
  })

  try {
    writeCanonicalJSON(requestPath, request)
    const product = spawnCanonicalIntegration(operatingSystem, {
      ...process.env,
      WINDSHARE_STABILITY_START_REQUEST: requestPath,
      WINDSHARE_STABILITY_STARTED_OUTPUT: startedOutput,
      WINDSHARE_STABILITY_START_SECRET: secret,
    })
    if (product.error !== undefined) {
      throw new Error('stability integration process could not be started', { cause: product.error })
    }

    if (!existsSync(startedOutput)) {
      // Setup, retained-executable settlement, and start-marker publication are
      // infrastructure prerequisites. Without their handshake no product sample exists.
      return Number.isSafeInteger(product.status) && product.status !== 0 ? product.status : 1
    }
    const observedStartedDocument = readFileSync(startedOutput, 'utf8')
    if (observedStartedDocument !== startedDocument) {
      throw new Error('stability integration started handshake disagrees with its request')
    }

    const result = createStabilityResult({
      ...identity,
      invocationId,
      startedEventSha256: stabilityEvidenceDigest(observedStartedDocument),
      startedExecutionContractSemanticSha256: started.execution_contract_semantic_sha256,
      productVerdict: createProductVerdictForTermination(product.status, product.signal),
      executionContract,
    })
    writeCanonicalJSON(output, result)
    process.stdout.write(`stability-result-event: ${JSON.stringify(result)}\n`)
    return Number.isSafeInteger(product.status) ? product.status : 1
  } finally {
    rmSync(requestRoot, { recursive: true, force: true })
  }
}

function publishStartedEvent() {
  const requestPath = requireEnvironmentPath(
    process.env.WINDSHARE_STABILITY_START_REQUEST,
    'start request',
  )
  const outputPath = requireEnvironmentPath(
    process.env.WINDSHARE_STABILITY_STARTED_OUTPUT,
    'started output',
  )
  const secret = process.env.WINDSHARE_STABILITY_START_SECRET
  if (typeof secret !== 'string' || !/^[a-f0-9]{64}$/u.test(secret)) {
    throw new Error('stability start handshake secret is missing or invalid')
  }

  const request = parseJSONObject(readFileSync(requestPath, 'utf8'), 'stability start request')
  requireExactFields(request, [
    'schema_version',
    'event',
    'authentication_tag',
  ], 'stability start request')
  if (request.schema_version !== STABILITY_START_REQUEST_SCHEMA_VERSION) {
    throw new Error('stability start request schema version is unsupported')
  }
  const event = parseStabilityStartedEvent(request.event)
  const document = `${JSON.stringify(event)}\n`
  const expectedTag = authenticateStartedEvent(secret, document)
  if (
    typeof request.authentication_tag !== 'string' ||
    !/^[a-f0-9]{64}$/u.test(request.authentication_tag) ||
    !timingSafeEqual(Buffer.from(request.authentication_tag, 'hex'), Buffer.from(expectedTag, 'hex'))
  ) {
    throw new Error('stability start request authentication failed')
  }
  writeCanonicalJSON(outputPath, event)
  process.stdout.write(`stability-result-event: ${JSON.stringify(event)}\n`)
}

function authenticateStartedEvent(secret, document) {
  return createHmac('sha256', Buffer.from(secret, 'hex'))
    .update(document, 'utf8')
    .digest('hex')
}

function requireEnvironmentPath(value, label) {
  if (typeof value !== 'string' || value === '' || value.includes('\0')) {
    throw new Error(`stability ${label} path is missing or invalid`)
  }
  return resolve(value)
}

function spawnCanonicalIntegration(operatingSystem, environment) {
  if (operatingSystem === 'linux') {
    return spawnSync('bash', ['scripts/ci/linux/integration.sh'], {
      cwd: process.cwd(),
      env: environment,
      stdio: 'inherit',
      windowsHide: true,
    })
  }
  return spawnSync('pwsh', [
    '-NoLogo',
    '-NoProfile',
    '-NonInteractive',
    '-File',
    'scripts/ci/windows/integration.ps1',
  ], {
    cwd: process.cwd(),
    env: environment,
    stdio: 'inherit',
    windowsHide: true,
  })
}

function asBuffer(value) {
  if (Buffer.isBuffer(value)) return value
  if (value instanceof Uint8Array) return Buffer.from(value.buffer, value.byteOffset, value.byteLength)
  if (value instanceof ArrayBuffer) return Buffer.from(value)
  throw new Error('stability evidence digest input must be bytes or text')
}

function main() {
  const [command, ...arguments_] = process.argv.slice(2)
  if (command === 'run') {
    process.exitCode = runIntegration(arguments_)
    return
  }
  if (command === 'started' && arguments_.length === 0) {
    publishStartedEvent()
    return
  }
  throw new Error('usage: result.mjs run ... | result.mjs started')
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    main()
  } catch (cause) {
    process.stderr.write(`stability-result: ${cause instanceof Error ? cause.message : String(cause)}\n`)
    process.exitCode = 1
  }
}
