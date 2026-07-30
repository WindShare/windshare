import {
  createHash,
  generateKeyPairSync,
  randomBytes,
  sign as signBytes,
} from 'node:crypto'
import { isAbsolute, join, resolve } from 'node:path'

import {
  PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS,
  PROCESS_SETTLEMENT_SCHEMA_VERSION,
  canonicalProcessSettlementPayloadBytes,
  processSettlementPublicKeyFingerprint,
  processSettlementSampleId,
} from '../../../../web/scripts/browser-evidence/artifact/settlement-receipt.ts'
import {
  D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES,
  PROCESS_SETTLEMENT_COMMAND_SCHEMA_VERSION,
  canonicalSampleCommandSha256 as canonicalSampleCommandAuthoritySha256,
  sampleDriverCommand,
} from './sample-command-authority.mjs'

export {
  D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES,
  PROCESS_SETTLEMENT_COMMAND_SCHEMA_VERSION,
  sampleDriverCommand,
}

const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const PORTABLE_TOKEN_PATTERN = /^[A-Za-z0-9._-]{1,128}$/u
const ED25519_NONCE_BYTES = 32
const MAXIMUM_COMMAND_ARGUMENTS = 4_096
const MAXIMUM_COMMAND_ARGUMENT_BYTES = 65_536

/** The same semantic authority builds both the launched argv and the guard's digest. */
export function sampleSupervisorArguments(authority, { ownershipEnvironment = {} } = {}) {
  return sampleDriverCommand(authority, { ownershipEnvironment }).arguments
}

export function canonicalSampleCommandSha256(authority) {
  return canonicalSampleCommandAuthoritySha256(authority)
}

export function createProcessSettlementSigner({
  invocationId = randomBytes(ED25519_NONCE_BYTES).toString('hex'),
  runtimeManifestSha256,
  now = Date.now,
  createKeyPair = () => generateKeyPairSync('ed25519'),
  createNonce = () => randomBytes(ED25519_NONCE_BYTES),
} = {}) {
  requirePortableToken(invocationId, 'process settlement invocation ID')
  requireSha256(runtimeManifestSha256, 'process settlement runtime manifest digest')
  if (typeof now !== 'function') throw new Error('process settlement clock must be a function')
  if (typeof createKeyPair !== 'function' || typeof createNonce !== 'function') {
    throw new Error('process settlement cryptographic providers must be functions')
  }
  const pair = createKeyPair()
  if (
    pair === null || typeof pair !== 'object' ||
    pair.publicKey?.asymmetricKeyType !== 'ed25519' ||
    pair.privateKey?.asymmetricKeyType !== 'ed25519'
  ) throw new Error('process settlement signer requires an Ed25519 key pair')
  const publicKeySpkiBase64 = pair.publicKey.export({ format: 'der', type: 'spki' }).toString('base64')
  const trust = Object.freeze({
    invocationId,
    runtimeManifestSha256,
    publicKeySpkiBase64,
    publicKeySha256: processSettlementPublicKeyFingerprint(publicKeySpkiBase64),
  })
  let privateKey = pair.privateKey

  return Object.freeze({
    trust,

    signSample({ sample, resultBytes, commandSha256, execution, ownershipBackend }) {
      if (privateKey === undefined) throw new Error('process settlement signer is retired')
      if (!(resultBytes instanceof Uint8Array)) {
        throw new Error('process settlement result snapshot must be bytes')
      }
      requireSha256(commandSha256, 'process settlement command digest')
      const issuedAtUnixMs = requireClock(now())
      const expiresAtUnixMs = issuedAtUnixMs + PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS
      if (!Number.isSafeInteger(expiresAtUnixMs)) {
        throw new Error('process settlement expiry exceeds the safe integer range')
      }
      const nonceBytes = createNonce()
      if (!(nonceBytes instanceof Uint8Array) || nonceBytes.byteLength !== ED25519_NONCE_BYTES) {
        throw new Error('process settlement nonce provider must return 256 bits')
      }
      const payload = Object.freeze({
        schemaVersion: PROCESS_SETTLEMENT_SCHEMA_VERSION,
        invocationId,
        sampleId: processSettlementSampleId(sample.suite, sample.browser, sample.sampleIndex),
        runId: sample.runId,
        runPolicy: sample.runPolicy,
        suite: sample.suite,
        browser: sample.browser,
        sampleIndex: sample.sampleIndex,
        checkoutSha: sample.checkoutSha,
        commandSha256,
        runtimeManifestSha256,
        resultSha256: createHash('sha256').update(resultBytes).digest('hex'),
        resultByteLength: String(resultBytes.byteLength),
        process: settlementProcessEvidence(execution),
        launched: requireBoolean(execution?.launched, 'process settlement launch evidence'),
        treeEmpty: requireBoolean(execution?.treeEmpty, 'process settlement tree-empty evidence'),
        input: settlementInputEvidence(execution),
        clientIo: settlementClientIoEvidence(execution),
        ownership: settlementOwnershipEvidence(execution, ownershipBackend),
        nonce: Buffer.from(nonceBytes).toString('hex'),
        issuedAtUnixMs: String(issuedAtUnixMs),
        expiresAtUnixMs: String(expiresAtUnixMs),
      })
      const canonical = canonicalProcessSettlementPayloadBytes(payload)
      return Object.freeze({
        payload,
        signatureBase64: signBytes(null, canonical, privateKey).toString('base64'),
      })
    },

    retire() {
      privateKey = undefined
    },
  })
}

function parseSampleCommandAuthority(value) {
  requireExactKeys(value, [
    'repository',
    'supervisor',
    'identity',
    'topology',
    'runtime',
    'output',
    'ownership',
    'leaf',
  ], 'sample command authority')
  const repository = parseRepositoryAuthority(value.repository)
  const supervisor = parseSupervisorAuthority(value.supervisor, repository)
  const identity = parseSampleIdentity(value.identity)
  const topology = parseTopologyAuthority(value.topology)
  const runtime = parseRuntimeAuthority(value.runtime)
  const output = parseOutputAuthority(value.output, identity)
  const ownership = parseOwnershipAuthority(value.ownership, identity, runtime)
  const leaf = parseLeafAuthority(value.leaf, supervisor)
  return Object.freeze({
    repository,
    supervisor,
    identity,
    topology,
    runtime,
    output,
    ownership,
    leaf,
  })
}

function parseRepositoryAuthority(value) {
  requireExactKeys(value, ['root', 'checkoutSha'], 'sample repository authority')
  const root = requireCanonicalAbsolutePath(value.root, 'sample repository root')
  if (!/^[a-f0-9]{40}$/u.test(value.checkoutSha)) {
    throw new Error('sample repository checkout SHA is invalid')
  }
  return Object.freeze({ root, checkoutSha: value.checkoutSha })
}

function parseSupervisorAuthority(value, repository) {
  requireExactKeys(value, [
    'nodeExecutable',
    'nodeExecutableSha256',
    'evidenceCliPath',
    'evidenceCliSha256',
    'environment',
  ], 'sample supervisor authority')
  const evidenceCliPath = requireCanonicalAbsolutePath(
    value.evidenceCliPath,
    'sample supervisor evidence CLI',
  )
  if (evidenceCliPath !== join(repository.root, 'web', 'scripts', 'browser-evidence', 'cli.ts')) {
    throw new Error('sample supervisor evidence CLI differs from the repository authority')
  }
  return Object.freeze({
    executable: requireCanonicalAbsolutePath(
      value.nodeExecutable,
      'sample supervisor Node executable',
    ),
    executableSha256: requireSha256(
      value.nodeExecutableSha256,
      'sample supervisor Node executable digest',
    ),
    evidenceCliPath,
    evidenceCliSha256: requireSha256(
      value.evidenceCliSha256,
      'sample supervisor evidence CLI digest',
    ),
    cwd: repository.root,
    cleanEnvironment: true,
    environment: canonicalEnvironment(value.environment),
  })
}

function parseSampleIdentity(value) {
  requireExactKeys(value, [
    'runId',
    'runPolicy',
    'suite',
    'browser',
    'sampleIndex',
  ], 'sample command identity')
  requirePortableToken(value.runId, 'sample command run ID')
  if (!['main', 'pion'].includes(value.suite)) throw new Error('sample command suite is invalid')
  if (!['chromium', 'firefox', 'webkit'].includes(value.browser)) {
    throw new Error('sample command browser is invalid')
  }
  const runPolicy = canonicalRunPolicy(value.runPolicy)
  if (
    !Number.isSafeInteger(value.sampleIndex) ||
    value.sampleIndex < 1 ||
    value.sampleIndex > runPolicy.sampleCount
  ) throw new Error('sample command index exceeds its run policy')
  return Object.freeze({
    runId: value.runId,
    runPolicy,
    suite: value.suite,
    browser: value.browser,
    sampleIndex: value.sampleIndex,
  })
}

function parseTopologyAuthority(value) {
  requireExactKeys(value, [
    'profilePath',
    'profileSha256',
    'resolutionPath',
    'resolutionSha256',
  ], 'sample command topology authority')
  return Object.freeze({
    profilePath: requireCanonicalAbsolutePath(value.profilePath, 'sample topology profile'),
    profileSha256: requireSha256(value.profileSha256, 'sample topology profile digest'),
    resolutionPath: requireCanonicalAbsolutePath(value.resolutionPath, 'sample topology resolution'),
    resolutionSha256: requireSha256(value.resolutionSha256, 'sample topology resolution digest'),
  })
}

function parseRuntimeAuthority(value) {
  requireExactKeys(value, [
    'manifestPath',
    'manifestSha256',
    'windowsJobHelper',
  ], 'sample command runtime authority')
  let windowsJobHelper = null
  if (value.windowsJobHelper !== null) {
    requireExactKeys(value.windowsJobHelper, ['path', 'sha256'], 'sample Windows Job helper')
    windowsJobHelper = Object.freeze({
      path: requireCanonicalAbsolutePath(value.windowsJobHelper.path, 'sample Windows Job helper'),
      sha256: requireSha256(value.windowsJobHelper.sha256, 'sample Windows Job helper digest'),
    })
  }
  return Object.freeze({
    manifestPath: requireCanonicalAbsolutePath(value.manifestPath, 'sample runtime manifest'),
    manifestSha256: requireSha256(value.manifestSha256, 'sample runtime manifest digest'),
    windowsJobHelper,
  })
}

function parseOutputAuthority(value, identity) {
  requireExactKeys(value, ['root', 'sampleDirectory', 'resultPath'], 'sample output authority')
  const root = requireCanonicalAbsolutePath(value.root, 'sample output root')
  const sampleDirectory = requireCanonicalAbsolutePath(
    value.sampleDirectory,
    'sample output directory',
  )
  const expectedDirectory = join(
    root,
    identity.suite,
    identity.browser,
    'sample-' + identity.sampleIndex,
  )
  if (sampleDirectory !== expectedDirectory || value.resultPath !== join(sampleDirectory, 'result.json')) {
    throw new Error('sample output authority differs from its identity slot')
  }
  return Object.freeze({ root, sampleDirectory, resultPath: value.resultPath })
}

function parseOwnershipAuthority(value, identity, runtime) {
  requireExactKeys(value, [
    'platform',
    'insideWindowsD5',
    'backend',
    'operationClass',
    'classDeadlineMs',
    'childDeadlineMs',
  ], 'sample process ownership authority')
  if (!['linux', 'darwin', 'win32'].includes(value.platform)) {
    throw new Error('sample ownership platform is invalid')
  }
  requireBoolean(value.insideWindowsD5, 'sample D5 ownership evidence')
  const expectedBackend = value.platform === 'win32' ? 'windows-job' : 'native-process-group'
  if (value.backend !== expectedBackend || value.operationClass !== 'browser-sample') {
    throw new Error('sample ownership backend or deadline class is invalid')
  }
  if ((value.platform === 'win32') !== (runtime.windowsJobHelper !== null)) {
    throw new Error('sample Windows Job helper differs from its ownership backend')
  }
  if (value.insideWindowsD5 && (value.platform !== 'win32' || identity.suite !== 'main')) {
    throw new Error('sample D5 ownership is valid only for Windows main')
  }
  for (const [entry, label] of [
    [value.classDeadlineMs, 'sample class deadline'],
    [value.childDeadlineMs, 'sample child deadline'],
  ]) {
    if (!Number.isSafeInteger(entry) || entry < 1) throw new Error(label + ' is invalid')
  }
  return Object.freeze({
    platform: value.platform,
    insideWindowsD5: value.insideWindowsD5,
    backend: value.backend,
    operationClass: value.operationClass,
    classDeadlineMs: value.classDeadlineMs,
    childDeadlineMs: value.childDeadlineMs,
  })
}

function parseLeafAuthority(value, supervisor) {
  requireExactKeys(value, [
    'executable',
    'entrypointSha256',
    'args',
    'cwd',
    'environment',
  ], 'sample leaf command authority')
  const executable = requireCanonicalAbsolutePath(value.executable, 'sample leaf executable')
  if (executable !== supervisor.executable) {
    throw new Error('sample leaf executable differs from its owned Node supervisor')
  }
  return Object.freeze({
    executable,
    entrypointSha256: requireSha256(value.entrypointSha256, 'sample leaf entrypoint digest'),
    args: canonicalArguments(value.args),
    cwd: requireCanonicalAbsolutePath(value.cwd, 'sample leaf working directory'),
    environment: canonicalEnvironment(value.environment),
  })
}

function supervisorArguments(authority, ownershipEnvironment) {
  const { identity, topology, runtime, output, ownership, leaf } = authority
  const explicitEnvironment = {
    ...leaf.environment,
    ...ownershipEnvironment,
  }
  return canonicalArguments([
    authority.supervisor.evidenceCliPath,
    'run-sample',
    '--run-id', identity.runId,
    '--run-policy', identity.runPolicy.policyId,
    '--suite', identity.suite,
    '--browser', identity.browser,
    '--sample', String(identity.sampleIndex),
    '--checkout-sha', authority.repository.checkoutSha,
    '--output-root', output.root,
    '--deadline-ms', String(ownership.childDeadlineMs),
    '--profile', topology.profilePath,
    '--profile-sha256', topology.profileSha256,
    '--resolution', topology.resolutionPath,
    '--resolution-sha256', topology.resolutionSha256,
    ...(runtime.windowsJobHelper === null
      ? []
      : ['--windows-job-helper', runtime.windowsJobHelper.path]),
    '--executable', leaf.executable,
    ...leaf.args.flatMap((argument) => ['--arg', argument]),
    '--cwd', leaf.cwd,
    ...Object.entries(canonicalEnvironment(explicitEnvironment)).flatMap(([name, entry]) => [
      '--env',
      name + '=' + entry,
    ]),
  ])
}

function actualOwnershipEnvironment(insideWindowsD5, value) {
  const environment = canonicalEnvironment(value)
  const names = Object.keys(environment)
  const expected = insideWindowsD5 ? D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES : []
  if (
    names.length !== expected.length ||
    expected.some((name) => !Object.hasOwn(environment, name) || environment[name] === '')
  ) throw new Error('sample D5 ownership environment is missing, unexpected, or duplicated')
  return environment
}

function canonicalRunPolicy(value) {
  requireExactKeys(
    value,
    ['schemaVersion', 'policyId', 'policyVersion', 'sampleCount'],
    'sample command run policy',
  )
  const counts = { blocking: 1, closure: 3, stability: 5 }
  if (
    value.schemaVersion !== 1 || value.policyVersion !== 1 ||
    !Object.hasOwn(counts, value.policyId) || value.sampleCount !== counts[value.policyId]
  ) throw new Error('sample command run policy is non-canonical')
  return Object.freeze({
    schemaVersion: 1,
    policyId: value.policyId,
    policyVersion: 1,
    sampleCount: value.sampleCount,
  })
}

function canonicalArguments(value) {
  if (
    !Array.isArray(value) || value.length > MAXIMUM_COMMAND_ARGUMENTS ||
    value.some((argument) =>
      typeof argument !== 'string' ||
      Buffer.byteLength(argument, 'utf8') > MAXIMUM_COMMAND_ARGUMENT_BYTES ||
      argument.includes('\0'))
  ) throw new Error('sample command arguments are not canonical bounded strings')
  return Object.freeze([...value])
}

function settlementProcessEvidence(execution) {
  if (execution === null || typeof execution !== 'object') {
    throw new Error('process settlement execution evidence is required')
  }
  const evidence = execution.processEvidence
  const timedOut = requireBoolean(execution.timedOut, 'process settlement timeout evidence')
  if (evidence?.terminal === 'exited' && Number.isSafeInteger(evidence.exitCode)) {
    return Object.freeze({ terminal: 'exited', timedOut, exitCode: evidence.exitCode })
  }
  if (evidence?.terminal === 'signaled' && typeof evidence.signal === 'string') {
    return Object.freeze({ terminal: 'signaled', timedOut, signal: evidence.signal })
  }
  if (
    evidence?.terminal === 'spawn-failed' &&
    typeof evidence.errorCode === 'string' &&
    typeof evidence.errorMessage === 'string'
  ) {
    return Object.freeze({
      terminal: 'spawn-failed',
      timedOut,
      errorCode: evidence.errorCode,
      errorMessage: evidence.errorMessage,
    })
  }
  throw new Error('process settlement terminal evidence is invalid')
}

function settlementInputEvidence(execution) {
  const evidence = requireEvidenceRecord(execution?.inputEvidence, 'process settlement input evidence')
  return Object.freeze({
    outcome: evidence.outcome,
    failureCode: evidence.failureCode,
    failureMessage: evidence.failureMessage,
  })
}

function settlementClientIoEvidence(execution) {
  const evidence = requireEvidenceRecord(
    execution?.clientIoEvidence,
    'process settlement client I/O evidence',
  )
  return Object.freeze({
    requestOutcome: evidence.requestOutcome,
    rawInputOutcome: evidence.rawInputOutcome,
    controlOutcome: evidence.controlOutcome,
    outputOutcome: evidence.outputOutcome,
    failureCode: evidence.failureCode,
    failureMessage: evidence.failureMessage,
  })
}

function settlementOwnershipEvidence(execution, backend) {
  const evidence = requireEvidenceRecord(
    execution?.ownershipEvidence,
    'process settlement ownership evidence',
  )
  if (backend === 'linux-subreaper') {
    return Object.freeze({
      backend,
      ownerPid: evidence.ownerPid,
      rootPid: evidence.rootPid,
      rootStartTimeTicks: evidence.rootStartTimeTicks,
      inventoryScans: evidence.inventoryScans,
      maximumObservedDescendants: evidence.maximumObservedDescendants,
      quietInventoryCount: evidence.quietInventoryCount,
      controlOutcome: evidence.controlOutcome,
      cleanupOutcome: evidence.cleanupOutcome,
      failureCode: evidence.failureCode,
      failureMessage: evidence.failureMessage,
    })
  }
  if (backend === 'windows-job') {
    const root = evidence.root === null
      ? null
      : Object.freeze({ pid: evidence.root?.pid, exitCode: evidence.root?.exitCode })
    return Object.freeze({
      backend,
      supervisionOutcome: evidence.supervisionOutcome,
      terminationReason: evidence.terminationReason,
      activeProcessCount: evidence.activeProcessCount,
      root,
      spawnFailure: evidence.spawnFailure,
    })
  }
  throw new Error('process settlement ownership backend is invalid')
}

function requireEvidenceRecord(value, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(label + ' is required')
  }
  return value
}

function requireCanonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(label + ' must be canonical and absolute')
  }
  return value
}

function requirePortableToken(value, label) {
  if (typeof value !== 'string' || !PORTABLE_TOKEN_PATTERN.test(value)) {
    throw new Error(label + ' is not a portable token')
  }
  return value
}

function requireSha256(value, label) {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) {
    throw new Error(label + ' is not a canonical SHA-256 digest')
  }
  return value
}

function requireBoolean(value, label) {
  if (typeof value !== 'boolean') throw new Error(label + ' must be boolean')
  return value
}

function requireClock(value) {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error('process settlement clock must return a non-negative safe integer')
  }
  return value
}

function requireExactKeys(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(label + ' must be an object')
  }
  const actual = Object.keys(value).sort()
  const canonical = [...expected].sort()
  if (
    actual.length !== canonical.length ||
    actual.some((name, index) => name !== canonical[index])
  ) throw new Error(label + ' has an invalid field set')
}

function canonicalEnvironment(value) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('sample command environment must be an object')
  }
  const entries = Object.entries(value).sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0)
  const names = new Set()
  const environment = {}
  for (const [name, entry] of entries) {
    const folded = name.toUpperCase()
    if (
      !/^[A-Za-z_][A-Za-z0-9_]*$/u.test(name) || names.has(folded) ||
      typeof entry !== 'string' || entry.includes('\0')
    ) throw new Error('sample command environment contains an invalid or ambiguous entry')
    names.add(folded)
    environment[name] = entry
  }
  return environment
}
