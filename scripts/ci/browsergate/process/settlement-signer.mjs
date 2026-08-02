import {
  createHash,
  generateKeyPairSync,
  randomBytes,
  sign as signBytes,
} from 'node:crypto'

import {
  PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS,
  PROCESS_SETTLEMENT_SCHEMA_VERSION,
  canonicalProcessSettlementPayloadBytes,
  processSettlementPublicKeyFingerprint,
  processSettlementSampleId,
} from '../../../../web/scripts/browser-evidence/artifact/settlement-receipt.ts'
import {
  PROCESS_SETTLEMENT_COMMAND_SCHEMA_VERSION,
  canonicalSampleCommandSha256 as canonicalSampleCommandAuthoritySha256,
  sampleDriverCommand,
} from './sample-command-authority.mjs'

export { PROCESS_SETTLEMENT_COMMAND_SCHEMA_VERSION, sampleDriverCommand }

const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const PORTABLE_TOKEN_PATTERN = /^[A-Za-z0-9._-]{1,128}$/u
const ED25519_NONCE_BYTES = 32
const OWNER_BACKENDS = Object.freeze(['linux_subreaper', 'windows_job'])
const TERMINATION_REASONS = Object.freeze([
  'natural',
  'deadline',
  'stop',
  'parent_lost',
  'initialization_failed',
  'owner_failure',
])

/** The launched command and settlement digest share one semantic authority. */
export function sampleSupervisorArguments(authority) {
  return sampleDriverCommand(authority).arguments
}

export function canonicalSampleCommandSha256(authority) {
  return canonicalSampleCommandAuthoritySha256(authority)
}

export function createProcessSettlementSigner({
  invocationId = randomBytes(ED25519_NONCE_BYTES).toString('hex'),
  now = Date.now,
  createKeyPair = () => generateKeyPairSync('ed25519'),
  createNonce = () => randomBytes(ED25519_NONCE_BYTES),
} = {}) {
  requirePortableToken(invocationId, 'process settlement invocation ID')
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
        resultSha256: createHash('sha256').update(resultBytes).digest('hex'),
        resultByteLength: String(resultBytes.byteLength),
        process: settlementProcessEvidence(execution),
        treeEmpty: requireBoolean(execution?.treeEmpty, 'process settlement tree-empty evidence'),
        cleanupOutcome: settlementCleanupOutcome(execution),
        input: settlementInputEvidence(execution),
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

function settlementProcessEvidence(execution) {
  const evidence = requireEvidenceRecord(execution?.processEvidence, 'process settlement terminal evidence')
  if (evidence.terminal === 'exited' && Number.isSafeInteger(evidence.exitCode)) {
    return Object.freeze({ terminal: 'exited', exitCode: evidence.exitCode })
  }
  if (evidence.terminal === 'signaled' && typeof evidence.signal === 'string') {
    return Object.freeze({ terminal: 'signaled', signal: evidence.signal })
  }
  if (
    evidence.terminal === 'spawn-failed' && typeof evidence.errorCode === 'string' &&
    typeof evidence.errorMessage === 'string'
  ) {
    return Object.freeze({
      terminal: 'spawn-failed',
      errorCode: evidence.errorCode,
      errorMessage: evidence.errorMessage,
    })
  }
  throw new Error('process settlement terminal evidence is invalid')
}

function settlementCleanupOutcome(execution) {
  if (!['completed', 'failed'].includes(execution?.cleanupOutcome)) {
    throw new Error('process settlement cleanup outcome is invalid')
  }
  return execution.cleanupOutcome
}

function settlementInputEvidence(execution) {
  const evidence = requireEvidenceRecord(execution?.inputEvidence, 'process settlement input evidence')
  return Object.freeze({
    outcome: evidence.outcome,
    failureCode: evidence.failureCode,
    failureMessage: evidence.failureMessage,
  })
}

function settlementOwnershipEvidence(execution, expectedBackend) {
  const evidence = requireEvidenceRecord(
    execution?.ownershipEvidence,
    'process settlement ownership evidence',
  )
  if (!OWNER_BACKENDS.includes(expectedBackend) || evidence.backend !== expectedBackend) {
    throw new Error('process settlement ownership backend differs from its outer authority')
  }
  if (evidence.kind !== 'test-process-owner' || !TERMINATION_REASONS.includes(evidence.terminationReason)) {
    throw new Error('process settlement ownership evidence is invalid')
  }
  return Object.freeze({
    kind: evidence.kind,
    backend: evidence.backend,
    terminationReason: evidence.terminationReason,
  })
}

function requireEvidenceRecord(value, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(label + ' is required')
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
