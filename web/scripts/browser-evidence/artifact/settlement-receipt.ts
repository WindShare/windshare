import { createHash, createPublicKey, verify as verifySignature } from 'node:crypto'

import {
  freezeRecord,
  requireBoolean,
  requireCheckoutSha,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireSha256,
  requireString,
} from '../contract/json.ts'
import type { BrowserSampleResult } from '../result.ts'
import {
  assertBrowserRunPolicyEqual,
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
  type BrowserRunPolicy,
} from '../run-policy.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
  type BrowserEngine,
  type BrowserSuite,
} from '../vocabulary.ts'

export const PROCESS_SETTLEMENT_SCHEMA_VERSION =
  'windshare.process-settlement/v2' as const
export const PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS = 21_600_000 as const
export const PROCESS_SETTLEMENT_CLOCK_SKEW_MS = 300_000 as const

const PROCESS_TERMINALS = Object.freeze(['exited', 'signaled', 'spawn-failed'] as const)
const CLEANUP_OUTCOMES = Object.freeze(['completed', 'failed'] as const)
const MAXIMUM_RESULT_BYTES = 16_777_216
const MAXIMUM_PUBLIC_KEY_BYTES = 1_024
const ED25519_SIGNATURE_BYTES = 64
const RECEIPT_NONCE = /^[a-f0-9]{64}$/u

export type ProcessSettlementTerminal = typeof PROCESS_TERMINALS[number]
export type ProcessSettlementCleanupOutcome = typeof CLEANUP_OUTCOMES[number]

export interface ProcessSettlementInputEvidence {
  readonly outcome: 'not-started' | 'not-requested' | 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface ProcessSettlementClientIoEvidence {
  readonly requestOutcome: 'delivered' | 'failed'
  readonly rawInputOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly controlOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly outputOutcome: 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export type ProcessSettlementOwnershipEvidence =
  | {
      readonly backend: 'linux-subreaper'
      readonly ownerPid: number
      readonly rootPid: number | null
      readonly rootStartTimeTicks: string
      readonly inventoryScans: number
      readonly maximumObservedDescendants: number
      readonly quietInventoryCount: number
      readonly controlOutcome: string
      readonly cleanupOutcome: ProcessSettlementCleanupOutcome
      readonly failureCode: string
      readonly failureMessage: string
    }
  | {
      readonly backend: 'windows-job'
      readonly supervisionOutcome: 'tree-empty' | 'spawn-failed'
      readonly terminationReason: 'natural' | 'target-spawn-failed' | 'deadline' | 'parent-request'
      readonly activeProcessCount: 0
      readonly root: { readonly pid: number; readonly exitCode: number } | null
      readonly spawnFailure: string | null
    }

export type ProcessSettlementEvidence =
  | {
      readonly terminal: 'exited'
      readonly timedOut: boolean
      readonly exitCode: number
    }
  | {
      readonly terminal: 'signaled'
      readonly timedOut: boolean
      readonly signal: string
    }
  | {
      readonly terminal: 'spawn-failed'
      readonly timedOut: boolean
      readonly errorCode: string
      readonly errorMessage: string
    }

export interface ProcessSettlementPayload {
  readonly schemaVersion: typeof PROCESS_SETTLEMENT_SCHEMA_VERSION
  readonly invocationId: string
  readonly sampleId: string
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
  readonly commandSha256: string
  readonly runtimeManifestSha256: string
  readonly resultSha256: string
  readonly resultByteLength: string
  readonly process: ProcessSettlementEvidence
  readonly launched: boolean
  readonly treeEmpty: boolean
  readonly input: ProcessSettlementInputEvidence
  readonly clientIo: ProcessSettlementClientIoEvidence
  readonly ownership: ProcessSettlementOwnershipEvidence
  readonly nonce: string
  readonly issuedAtUnixMs: string
  readonly expiresAtUnixMs: string
}

export interface ProcessSettlementAttestation {
  readonly payload: ProcessSettlementPayload
  readonly signatureBase64: string
}

export interface ProcessSettlementTrustAnchor {
  readonly invocationId: string
  readonly runtimeManifestSha256: string
  readonly publicKeySpkiBase64: string
  readonly publicKeySha256: string
}

export interface ProcessSettlementSampleExpectation {
  readonly sample: Pick<
    BrowserSampleResult,
    'runId' | 'runPolicy' | 'suite' | 'browser' | 'sampleIndex' | 'checkoutSha'
  >
  readonly resultBytes: Uint8Array
  readonly commandSha256: string
}

export interface VerifyProcessSettlementOptions {
  readonly trust: ProcessSettlementTrustAnchor
  readonly samples: readonly ProcessSettlementSampleExpectation[]
  readonly attestations: readonly unknown[]
  readonly nowUnixMs?: number
}

declare const VERIFIED_PROCESS_SETTLEMENT_BRAND: unique symbol

export interface VerifiedProcessSettlementSet {
  readonly invocationId: string
  readonly sampleKeys: readonly string[]
  readonly inventorySha256: string
  readonly [VERIFIED_PROCESS_SETTLEMENT_BRAND]: true
}

const verifiedSettlementSets = new WeakSet<object>()

export function processSettlementSampleId(
  suite: BrowserSuite,
  browser: BrowserEngine,
  sampleIndex: number,
): string {
  return `${suite}-${browser}-sample-${sampleIndex}`
}

/** Used by the trusted signer so signature bytes and verifier bytes cannot drift. */
export function canonicalProcessSettlementPayloadBytes(value: unknown): Uint8Array {
  return Buffer.from(JSON.stringify(parseProcessSettlementPayload(value)), 'utf8')
}

export function processSettlementPublicKeyFingerprint(publicKeySpkiBase64: string): string {
  return createHash('sha256')
    .update(decodeCanonicalBase64(publicKeySpkiBase64, MAXIMUM_PUBLIC_KEY_BYTES, 'settlement public key'))
    .digest('hex')
}

export function verifyProcessSettlementAttestations(
  options: VerifyProcessSettlementOptions,
): VerifiedProcessSettlementSet {
  const now = requireVerificationTime(options.nowUnixMs)
  const trust = parseTrustAnchor(options.trust)
  const publicKey = verifiedSettlementPublicKey(trust)
  if (options.samples.length < 1 || options.attestations.length !== options.samples.length) {
    throw new Error('process settlement inventory is empty, missing, or contains extras')
  }

  const attestations = indexSettlementAttestations(options.attestations)
  const sampleKeys = new Set<string>()
  const sampleIds = new Set<string>()
  const nonces = new Set<string>()
  const verifiedKeys: string[] = []
  for (const expected of options.samples) {
    verifiedKeys.push(verifyExpectedSettlement({
      expected,
      trust,
      now,
      publicKey,
      attestations,
      sampleKeys,
      sampleIds,
      nonces,
    }))
  }
  verifiedKeys.sort(compareStrings)
  const capability = Object.freeze({
    invocationId: trust.invocationId,
    sampleKeys: Object.freeze(verifiedKeys),
    inventorySha256: settlementInventorySha256(options.samples),
  }) as VerifiedProcessSettlementSet
  verifiedSettlementSets.add(capability)
  return capability
}

function requireVerificationTime(value: number | undefined): number {
  const now = value ?? Date.now()
  if (!Number.isSafeInteger(now) || now < 0) {
    throw new Error('process settlement verification time is invalid')
  }
  return now
}

function verifiedSettlementPublicKey(
  trust: ProcessSettlementTrustAnchor,
): ReturnType<typeof createPublicKey> {
  const keyBytes = decodeCanonicalBase64(
    trust.publicKeySpkiBase64,
    MAXIMUM_PUBLIC_KEY_BYTES,
    'settlement public key',
  )
  if (createHash('sha256').update(keyBytes).digest('hex') !== trust.publicKeySha256) {
    throw new Error('process settlement public key differs from its pinned fingerprint')
  }
  const publicKey = createPublicKey({ key: keyBytes, format: 'der', type: 'spki' })
  if (publicKey.asymmetricKeyType !== 'ed25519') {
    throw new Error('process settlement trust anchor is not an Ed25519 public key')
  }
  return publicKey
}

function indexSettlementAttestations(
  values: readonly unknown[],
): ReadonlyMap<string, ProcessSettlementAttestation> {
  const attestations = new Map<string, ProcessSettlementAttestation>()
  for (const value of values) {
    const attestation = parseProcessSettlementAttestation(value)
    const key = sampleKey(attestation.payload)
    if (attestations.has(key)) throw new Error(`process settlement attestation ${key} is duplicated`)
    attestations.set(key, attestation)
  }
  return attestations
}

function verifyExpectedSettlement(options: {
  readonly expected: ProcessSettlementSampleExpectation
  readonly trust: ProcessSettlementTrustAnchor
  readonly now: number
  readonly publicKey: ReturnType<typeof createPublicKey>
  readonly attestations: ReadonlyMap<string, ProcessSettlementAttestation>
  readonly sampleKeys: Set<string>
  readonly sampleIds: Set<string>
  readonly nonces: Set<string>
}): string {
  const key = sampleKey(options.expected.sample)
  if (options.sampleKeys.has(key)) throw new Error(`process settlement sample ${key} is duplicated`)
  options.sampleKeys.add(key)
  const attestation = options.attestations.get(key)
  if (attestation === undefined) throw new Error(`process settlement attestation ${key} is missing`)
  const payload = attestation.payload
  assertPayloadIdentity(payload, options.expected, options.trust)
  assertPayloadFreshness(payload, options.now)
  if (options.sampleIds.has(payload.sampleId) || options.nonces.has(payload.nonce)) {
    throw new Error('process settlement replayed a sample ID or nonce')
  }
  options.sampleIds.add(payload.sampleId)
  options.nonces.add(payload.nonce)
  const signature = decodeCanonicalBase64(
    attestation.signatureBase64,
    ED25519_SIGNATURE_BYTES,
    'process settlement signature',
    ED25519_SIGNATURE_BYTES,
  )
  if (!verifySignature(
    null,
    canonicalProcessSettlementPayloadBytes(payload),
    options.publicKey,
    signature,
  )) throw new Error(`process settlement signature is invalid for ${key}`)
  if (!isNormalAcceptedSettlement(payload)) {
    throw new Error(`process settlement did not prove clean quiescence for ${key}`)
  }
  return key
}

export function requireVerifiedProcessSettlementSet(
  value: unknown,
  expected: {
    readonly invocationId: string
    readonly samples: readonly ProcessSettlementSampleExpectation[]
  },
): VerifiedProcessSettlementSet {
  if ((typeof value !== 'object' && typeof value !== 'function') || value === null ||
      !verifiedSettlementSets.has(value)) {
    throw new Error('native publication lacks an exact verified process-settlement capability')
  }
  const capability = value as VerifiedProcessSettlementSet
  if (
    capability.invocationId !== expected.invocationId ||
    capability.inventorySha256 !== settlementInventorySha256(expected.samples) ||
    capability.sampleKeys.length !== expected.samples.length
  ) throw new Error('verified process-settlement capability belongs to another inventory')
  return capability
}

function parseProcessSettlementAttestation(value: unknown): ProcessSettlementAttestation {
  const record = requireRecord(value, 'process settlement attestation')
  requireExactKeys(record, ['payload', 'signatureBase64'], [], 'process settlement attestation')
  return freezeRecord({
    payload: parseProcessSettlementPayload(record.payload),
    signatureBase64: requireString(
      record.signatureBase64,
      'process settlement signature',
      128,
    ),
  })
}

function parseProcessSettlementPayload(value: unknown): ProcessSettlementPayload {
  const record = requireRecord(value, 'process settlement payload')
  requireExactKeys(record, [
    'schemaVersion',
    'invocationId',
    'sampleId',
    'runId',
    'runPolicy',
    'suite',
    'browser',
    'sampleIndex',
    'checkoutSha',
    'commandSha256',
    'runtimeManifestSha256',
    'resultSha256',
    'resultByteLength',
    'process',
    'launched',
    'treeEmpty',
    'input',
    'clientIo',
    'ownership',
    'nonce',
    'issuedAtUnixMs',
    'expiresAtUnixMs',
  ], [], 'process settlement payload')
  const suite = requireEnum(record.suite, BROWSER_SUITES, 'process settlement suite')
  const browser = requireEnum(record.browser, BROWSER_ENGINES, 'process settlement browser')
  const runPolicy = parseBrowserRunPolicy(record.runPolicy, 'process settlement run policy')
  const sampleIndex = validatePolicySampleIndex(
    requireSafeInteger(record.sampleIndex, 1, 1_000, 'process settlement sample index'),
    runPolicy,
    'process settlement sample index',
  )
  return freezeRecord({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      PROCESS_SETTLEMENT_SCHEMA_VERSION,
      'process settlement schema version',
    ),
    invocationId: requirePortableToken(record.invocationId, 'process settlement invocation ID'),
    sampleId: requirePortableToken(record.sampleId, 'process settlement sample ID'),
    runId: requirePortableToken(record.runId, 'process settlement run ID'),
    runPolicy,
    suite,
    browser,
    sampleIndex,
    checkoutSha: requireCheckoutSha(record.checkoutSha, 'process settlement checkout SHA'),
    commandSha256: requireSha256(record.commandSha256, 'process settlement command digest'),
    runtimeManifestSha256: requireSha256(
      record.runtimeManifestSha256,
      'process settlement runtime manifest digest',
    ),
    resultSha256: requireSha256(record.resultSha256, 'process settlement result digest'),
    resultByteLength: requireCanonicalUnsignedDecimal(
      record.resultByteLength,
      MAXIMUM_RESULT_BYTES,
      'process settlement result byte length',
    ),
    process: parseProcessEvidence(record.process),
    launched: requireBoolean(record.launched, 'process settlement launch evidence'),
    treeEmpty: requireBoolean(record.treeEmpty, 'process settlement tree-empty evidence'),
    input: parseInputEvidence(record.input),
    clientIo: parseClientIoEvidence(record.clientIo),
    ownership: parseOwnershipEvidence(record.ownership),
    nonce: requireNonce(record.nonce),
    issuedAtUnixMs: requireCanonicalUnsignedDecimal(
      record.issuedAtUnixMs,
      Number.MAX_SAFE_INTEGER,
      'process settlement issue time',
    ),
    expiresAtUnixMs: requireCanonicalUnsignedDecimal(
      record.expiresAtUnixMs,
      Number.MAX_SAFE_INTEGER,
      'process settlement expiry time',
    ),
  })
}

function parseProcessEvidence(value: unknown): ProcessSettlementEvidence {
  const record = requireRecord(value, 'process settlement terminal evidence')
  const terminal = requireEnum(record.terminal, PROCESS_TERMINALS, 'process settlement terminal')
  const timedOut = requireBoolean(record.timedOut, 'process settlement timeout evidence')
  if (terminal === 'exited') {
    requireExactKeys(record, ['terminal', 'timedOut', 'exitCode'], [], 'process settlement terminal evidence')
    return freezeRecord({
      terminal,
      timedOut,
      exitCode: requireSafeInteger(record.exitCode, 0, 0xffff_ffff, 'process settlement exit code'),
    })
  }
  if (terminal === 'signaled') {
    requireExactKeys(record, ['terminal', 'timedOut', 'signal'], [], 'process settlement terminal evidence')
    return freezeRecord({ terminal, timedOut, signal: requirePortableToken(record.signal, 'process signal') })
  }
  requireExactKeys(
    record,
    ['terminal', 'timedOut', 'errorCode', 'errorMessage'],
    [],
    'process settlement terminal evidence',
  )
  return freezeRecord({
    terminal,
    timedOut,
    errorCode: requirePortableToken(record.errorCode, 'process spawn error code'),
    errorMessage: requireString(record.errorMessage, 'process spawn error message', 512),
  })
}

function parseInputEvidence(value: unknown): ProcessSettlementInputEvidence {
  const record = requireRecord(value, 'process settlement input evidence')
  requireExactKeys(
    record,
    ['outcome', 'failureCode', 'failureMessage'],
    [],
    'process settlement input evidence',
  )
  const outcome = requireEnum(
    record.outcome,
    ['not-started', 'not-requested', 'delivered', 'failed'] as const,
    'process settlement input outcome',
  )
  const failureCode = requireOptionalPortableToken(
    record.failureCode,
    'process settlement input failure code',
  )
  const failureMessage = requireOptionalText(
    record.failureMessage,
    'process settlement input failure message',
    512,
  )
  if (
    outcome === 'failed'
      ? failureCode === '' || failureMessage === ''
      : failureCode !== '' || failureMessage !== ''
  ) throw new Error('process settlement input outcome contradicts its failure evidence')
  return freezeRecord({ outcome, failureCode, failureMessage })
}

function parseClientIoEvidence(value: unknown): ProcessSettlementClientIoEvidence {
  const record = requireRecord(value, 'process settlement client I/O evidence')
  requireExactKeys(record, [
    'requestOutcome',
    'rawInputOutcome',
    'controlOutcome',
    'outputOutcome',
    'failureCode',
    'failureMessage',
  ], [], 'process settlement client I/O evidence')
  const requestOutcome = requireEnum(
    record.requestOutcome,
    ['delivered', 'failed'] as const,
    'process settlement request I/O outcome',
  )
  const rawInputOutcome = requireEnum(
    record.rawInputOutcome,
    ['not-requested', 'delivered', 'failed'] as const,
    'process settlement raw-input I/O outcome',
  )
  const controlOutcome = requireEnum(
    record.controlOutcome,
    ['not-requested', 'delivered', 'failed'] as const,
    'process settlement control I/O outcome',
  )
  const outputOutcome = requireEnum(
    record.outputOutcome,
    ['delivered', 'failed'] as const,
    'process settlement output I/O outcome',
  )
  const failureCode = requireOptionalPortableToken(
    record.failureCode,
    'process settlement client I/O failure code',
  )
  const failureMessage = requireOptionalText(
    record.failureMessage,
    'process settlement client I/O failure message',
    512,
  )
  const failed = [requestOutcome, rawInputOutcome, controlOutcome, outputOutcome].includes('failed')
  if (
    failed
      ? failureCode === '' || failureMessage === ''
      : failureCode !== '' || failureMessage !== ''
  ) throw new Error('process settlement client I/O outcome contradicts its failure evidence')
  return freezeRecord({
    requestOutcome,
    rawInputOutcome,
    controlOutcome,
    outputOutcome,
    failureCode,
    failureMessage,
  })
}

function parseOwnershipEvidence(value: unknown): ProcessSettlementOwnershipEvidence {
  const record = requireRecord(value, 'process settlement ownership evidence')
  if (record.backend === 'linux-subreaper') return parseLinuxOwnershipEvidence(record)
  if (record.backend === 'windows-job') return parseWindowsOwnershipEvidence(record)
  throw new Error('process settlement ownership backend is invalid')
}

function parseLinuxOwnershipEvidence(
  record: Record<string, unknown>,
): Extract<ProcessSettlementOwnershipEvidence, { readonly backend: 'linux-subreaper' }> {
  requireExactKeys(record, [
    'backend',
    'ownerPid',
    'rootPid',
    'rootStartTimeTicks',
    'inventoryScans',
    'maximumObservedDescendants',
    'quietInventoryCount',
    'controlOutcome',
    'cleanupOutcome',
    'failureCode',
    'failureMessage',
  ], [], 'Linux process settlement ownership evidence')
  const rootPid = record.rootPid === null
    ? null
    : requireSafeInteger(record.rootPid, 1, 0x7fff_ffff, 'Linux settlement root PID')
  const rootStartTimeTicks = requireOptionalText(
    record.rootStartTimeTicks,
    'Linux settlement root starttime',
    32,
  )
  if ((rootPid === null) !== (rootStartTimeTicks === '')) {
    throw new Error('Linux settlement root identity is incomplete')
  }
  if (rootStartTimeTicks !== '' && !canonicalUint64(rootStartTimeTicks)) {
    throw new Error('Linux settlement root starttime is not canonical uint64')
  }
  const cleanupOutcome = requireEnum(
    record.cleanupOutcome,
    CLEANUP_OUTCOMES,
    'Linux settlement cleanup outcome',
  )
  const failureCode = requireOptionalPortableToken(
    record.failureCode,
    'Linux settlement ownership failure code',
  )
  const failureMessage = requireOptionalText(
    record.failureMessage,
    'Linux settlement ownership failure message',
    512,
  )
  if (
    cleanupOutcome === 'failed'
      ? failureCode === '' || failureMessage === ''
      : failureCode !== '' || failureMessage !== ''
  ) throw new Error('Linux settlement cleanup contradicts its failure evidence')
  return freezeRecord({
    backend: 'linux-subreaper' as const,
    ownerPid: requireSafeInteger(record.ownerPid, 1, 0x7fff_ffff, 'Linux settlement owner PID'),
    rootPid,
    rootStartTimeTicks,
    inventoryScans: requireSafeInteger(
      record.inventoryScans,
      0,
      Number.MAX_SAFE_INTEGER,
      'Linux settlement inventory scans',
    ),
    maximumObservedDescendants: requireSafeInteger(
      record.maximumObservedDescendants,
      0,
      Number.MAX_SAFE_INTEGER,
      'Linux settlement maximum descendants',
    ),
    quietInventoryCount: requireSafeInteger(
      record.quietInventoryCount,
      0,
      2,
      'Linux settlement quiet inventory count',
    ),
    controlOutcome: requirePortableToken(
      record.controlOutcome,
      'Linux settlement control outcome',
    ),
    cleanupOutcome,
    failureCode,
    failureMessage,
  })
}

function parseWindowsOwnershipEvidence(
  record: Record<string, unknown>,
): Extract<ProcessSettlementOwnershipEvidence, { readonly backend: 'windows-job' }> {
  requireExactKeys(record, [
    'backend',
    'supervisionOutcome',
    'terminationReason',
    'activeProcessCount',
    'root',
    'spawnFailure',
  ], [], 'Windows process settlement ownership evidence')
  let root: { readonly pid: number; readonly exitCode: number } | null = null
  if (record.root !== null) {
    const rootRecord = requireRecord(record.root, 'Windows settlement root evidence')
    requireExactKeys(rootRecord, ['pid', 'exitCode'], [], 'Windows settlement root evidence')
    root = freezeRecord({
      pid: requireSafeInteger(rootRecord.pid, 1, 0xffff_ffff, 'Windows settlement root PID'),
      exitCode: requireSafeInteger(
        rootRecord.exitCode,
        0,
        0xffff_ffff,
        'Windows settlement root exit code',
      ),
    })
  }
  return freezeRecord({
    backend: 'windows-job' as const,
    supervisionOutcome: requireEnum(
      record.supervisionOutcome,
      ['tree-empty', 'spawn-failed'] as const,
      'Windows settlement supervision outcome',
    ),
    terminationReason: requireEnum(
      record.terminationReason,
      ['natural', 'target-spawn-failed', 'deadline', 'parent-request'] as const,
      'Windows settlement termination reason',
    ),
    activeProcessCount: requireLiteral(
      record.activeProcessCount,
      0,
      'Windows settlement active process count',
    ),
    root,
    spawnFailure: record.spawnFailure === null
      ? null
      : requireString(record.spawnFailure, 'Windows settlement spawn failure', 512),
  })
}

function isNormalAcceptedSettlement(payload: ProcessSettlementPayload): boolean {
  if (
    payload.launched !== true || payload.treeEmpty !== true || payload.process.timedOut !== false ||
    payload.process.terminal === 'spawn-failed' ||
    payload.input.outcome !== 'delivered' || payload.input.failureCode !== '' ||
    payload.input.failureMessage !== '' || payload.clientIo.requestOutcome !== 'delivered' ||
    payload.clientIo.rawInputOutcome !== 'delivered' ||
    payload.clientIo.controlOutcome !== 'not-requested' ||
    payload.clientIo.outputOutcome !== 'delivered' || payload.clientIo.failureCode !== '' ||
    payload.clientIo.failureMessage !== ''
  ) return false
  if (payload.ownership.backend === 'linux-subreaper') {
    return payload.ownership.rootPid !== null && payload.ownership.rootStartTimeTicks !== '' &&
      payload.ownership.inventoryScans >= 2 && payload.ownership.quietInventoryCount === 2 &&
      payload.ownership.controlOutcome === 'target-terminal' &&
      payload.ownership.cleanupOutcome === 'completed' && payload.ownership.failureCode === '' &&
      payload.ownership.failureMessage === ''
  }
  return payload.ownership.supervisionOutcome === 'tree-empty' &&
    payload.ownership.terminationReason === 'natural' &&
    payload.ownership.activeProcessCount === 0 && payload.ownership.root !== null &&
    payload.ownership.spawnFailure === null
}

function parseTrustAnchor(value: ProcessSettlementTrustAnchor): ProcessSettlementTrustAnchor {
  const record = requireRecord(value, 'process settlement trust anchor')
  requireExactKeys(record, [
    'invocationId', 'runtimeManifestSha256', 'publicKeySpkiBase64', 'publicKeySha256',
  ], [], 'process settlement trust anchor')
  return freezeRecord({
    invocationId: requirePortableToken(record.invocationId, 'settlement invocation ID'),
    runtimeManifestSha256: requireSha256(
      record.runtimeManifestSha256,
      'settlement runtime manifest digest',
    ),
    publicKeySpkiBase64: requireString(record.publicKeySpkiBase64, 'settlement public key', 1_500),
    publicKeySha256: requireSha256(record.publicKeySha256, 'settlement public key fingerprint'),
  })
}

function assertPayloadIdentity(
  payload: ProcessSettlementPayload,
  expected: ProcessSettlementSampleExpectation,
  trust: ProcessSettlementTrustAnchor,
): void {
  const sample = expected.sample
  const resultSha256 = createHash('sha256').update(expected.resultBytes).digest('hex')
  const expectedIdentity = {
    invocationId: trust.invocationId,
    runtimeManifestSha256: trust.runtimeManifestSha256,
    sampleId: processSettlementSampleId(sample.suite, sample.browser, sample.sampleIndex),
    runId: sample.runId,
    suite: sample.suite,
    browser: sample.browser,
    sampleIndex: sample.sampleIndex,
    checkoutSha: sample.checkoutSha,
    commandSha256: requireSha256(expected.commandSha256, 'expected sample command digest'),
    resultSha256,
    resultByteLength: String(expected.resultBytes.byteLength),
  } as const
  const mismatches = Object.keys(expectedIdentity).filter((name) =>
    payload[name as keyof typeof expectedIdentity] !==
      expectedIdentity[name as keyof typeof expectedIdentity])
  if (mismatches.length > 0) {
    // Field names diagnose authority drift without serializing command data,
    // trust keys, nonces, or signatures into orchestration logs.
    throw new Error(
      `process settlement identity differs for ${sampleKey(sample)}: ${mismatches.join(',')}`,
    )
  }
  assertBrowserRunPolicyEqual(payload.runPolicy, sample.runPolicy, 'process settlement run policy')
}

function assertPayloadFreshness(payload: ProcessSettlementPayload, now: number): void {
  const issued = Number(payload.issuedAtUnixMs)
  const expires = Number(payload.expiresAtUnixMs)
  if (
    expires <= issued || expires - issued > PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS ||
    issued > now + PROCESS_SETTLEMENT_CLOCK_SKEW_MS || expires <= now
  ) throw new Error('process settlement receipt is expired or outside its bounded lifetime')
}

function settlementInventorySha256(
  samples: readonly ProcessSettlementSampleExpectation[],
): string {
  const inventory = samples.map((expected) => freezeRecord({
    sampleKey: sampleKey(expected.sample),
    resultSha256: createHash('sha256').update(expected.resultBytes).digest('hex'),
    resultByteLength: String(expected.resultBytes.byteLength),
    commandSha256: requireSha256(expected.commandSha256, 'expected sample command digest'),
  })).sort((left, right) => compareStrings(left.sampleKey, right.sampleKey))
  return createHash('sha256').update(JSON.stringify(inventory), 'utf8').digest('hex')
}

function requireCanonicalUnsignedDecimal(value: unknown, maximum: number, label: string): string {
  const encoded = requireString(value, label, 32)
  if (!/^(?:0|[1-9]\d*)$/u.test(encoded)) throw new Error(`${label} is not canonical unsigned decimal`)
  const numeric = Number(encoded)
  if (!Number.isSafeInteger(numeric) || numeric > maximum) throw new Error(`${label} exceeds its authority`)
  return encoded
}

function requireNonce(value: unknown): string {
  const nonce = requireString(value, 'process settlement nonce', 64)
  if (!RECEIPT_NONCE.test(nonce)) throw new Error('process settlement nonce is not canonical 256-bit hex')
  return nonce
}

function decodeCanonicalBase64(
  value: unknown,
  maximumBytes: number,
  label: string,
  exactBytes?: number,
): Buffer {
  const encoded = requireString(value, label, Math.ceil(maximumBytes / 3) * 4 + 4)
  const decoded = Buffer.from(encoded, 'base64')
  if (
    decoded.byteLength < 1 || decoded.byteLength > maximumBytes ||
    (exactBytes !== undefined && decoded.byteLength !== exactBytes) ||
    decoded.toString('base64') !== encoded
  ) throw new Error(`${label} is not canonical bounded base64`)
  return decoded
}

function requirePortableToken(value: unknown, label: string): string {
  const token = requireString(value, label, 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(token)) throw new Error(`${label} contains non-portable characters`)
  return token
}

function requireOptionalPortableToken(value: unknown, label: string): string {
  const token = requireOptionalText(value, label, 128)
  if (token !== '' && !/^[A-Za-z0-9._-]+$/u.test(token)) {
    throw new Error(`${label} contains non-portable characters`)
  }
  return token
}

function requireOptionalText(value: unknown, label: string, maximumUtf8Bytes: number): string {
  if (
    typeof value !== 'string' || value.normalize('NFC') !== value || value.includes('\0') ||
    Buffer.byteLength(value, 'utf8') > maximumUtf8Bytes
  ) throw new Error(`${label} must be optional bounded NFC text`)
  return value
}

function canonicalUint64(value: string): boolean {
  if (!/^[1-9]\d*$/u.test(value)) return false
  try {
    const parsed = BigInt(value)
    return parsed <= 0xffff_ffff_ffff_ffffn && parsed.toString(10) === value
  } catch {
    return false
  }
}

function sampleKey(sample: Pick<BrowserSampleResult, 'suite' | 'browser' | 'sampleIndex'>): string {
  return `${sample.suite}/${sample.browser}/${sample.sampleIndex}`
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
