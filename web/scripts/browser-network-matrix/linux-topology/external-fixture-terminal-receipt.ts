import { createHash, createPublicKey, verify } from 'node:crypto'

import {
  parseNetworkMatrixAttemptAuthority,
  type NetworkMatrixAttemptAuthority,
} from '../sample-authority.ts'
import {
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
  EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
} from './external-fixture-attestation.ts'

const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const SIGNATURE_PATTERN = /^[A-Za-z0-9_-]{86}$/u
const UTC_TIMESTAMP_PATTERN =
  /^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u

export const EXTERNAL_FIXTURE_MAXIMUM_ATTEMPT_LEASE_MS = 60_000

export interface ExternalFixtureTerminalReceipt {
  readonly protocolVersion: typeof EXTERNAL_FIXTURE_CONTROL_PROTOCOL
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly terminalAt: string
  readonly attemptLeaseIssuedAt: string
  readonly attemptLeaseExpiresAt: string
  readonly attemptLeaseMillis: number
  readonly state: 'established' | 'failed'
  readonly selectedPair: unknown | null
  readonly challengeBindingSha256: string
  readonly failureCode: string | null
}

export interface SignedExternalFixtureTerminalReceipt {
  readonly protocolVersion: typeof EXTERNAL_FIXTURE_CONTROL_PROTOCOL
  readonly receipt: ExternalFixtureTerminalReceipt
  readonly receiptSha256: string
  readonly signatureAlgorithm: typeof EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM
  readonly signature: string
}

export function parseSignedExternalFixtureTerminalReceipt(
  value: unknown,
): SignedExternalFixtureTerminalReceipt {
  const envelope = exactRecord(value, [
    'protocolVersion', 'receipt', 'receiptSha256', 'signatureAlgorithm', 'signature',
  ])
  if (
    envelope.protocolVersion !== EXTERNAL_FIXTURE_CONTROL_PROTOCOL ||
    envelope.signatureAlgorithm !== EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM
  ) invalidReceipt()
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    receipt: parseReceipt(envelope.receipt),
    receiptSha256: requireSha256(envelope.receiptSha256),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: requireSignature(envelope.signature),
  })
}

export function authenticateSignedExternalFixtureTerminalReceipt(
  value: SignedExternalFixtureTerminalReceipt,
  attestationPublicKey: string | Buffer,
): ExternalFixtureTerminalReceipt {
  const signed = parseSignedExternalFixtureTerminalReceipt(value)
  const canonical = canonicalExternalFixtureTerminalReceiptJson(signed.receipt)
  if (signed.receiptSha256 !== createHash('sha256').update(canonical).digest('hex')) invalidReceipt()
  let publicKey: ReturnType<typeof createPublicKey>
  try {
    publicKey = createPublicKey(attestationPublicKey)
  } catch {
    invalidReceipt()
  }
  if (
    publicKey.asymmetricKeyType !== 'ed25519' ||
    !verify(null, Buffer.from(canonical), publicKey, Buffer.from(signed.signature, 'base64url'))
  ) invalidReceipt()
  return signed.receipt
}

export function canonicalExternalFixtureTerminalReceiptJson(
  receipt: ExternalFixtureTerminalReceipt,
): string {
  return `${JSON.stringify(receipt)}\n`
}

function parseReceipt(value: unknown): ExternalFixtureTerminalReceipt {
  const receipt = exactRecord(value, [
    'protocolVersion', 'attemptAuthority', 'terminalAt', 'attemptLeaseIssuedAt',
    'attemptLeaseExpiresAt', 'attemptLeaseMillis', 'state', 'selectedPair', 'challengeBindingSha256',
    'failureCode',
  ])
  if (
    receipt.protocolVersion !== EXTERNAL_FIXTURE_CONTROL_PROTOCOL ||
    receipt.state !== 'established' && receipt.state !== 'failed' ||
    receipt.failureCode !== null && typeof receipt.failureCode !== 'string' ||
    receipt.state === 'established' && (
      receipt.selectedPair === null || receipt.failureCode !== null
    ) ||
    receipt.state === 'failed' && receipt.selectedPair !== null ||
    !Number.isSafeInteger(receipt.attemptLeaseMillis) ||
    (receipt.attemptLeaseMillis as number) < 1 ||
    (receipt.attemptLeaseMillis as number) > EXTERNAL_FIXTURE_MAXIMUM_ATTEMPT_LEASE_MS
  ) invalidReceipt()
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    attemptAuthority: parseNetworkMatrixAttemptAuthority(receipt.attemptAuthority),
    terminalAt: requireTimestamp(receipt.terminalAt),
    attemptLeaseIssuedAt: requireTimestamp(receipt.attemptLeaseIssuedAt),
    attemptLeaseExpiresAt: requireTimestamp(receipt.attemptLeaseExpiresAt),
    attemptLeaseMillis: receipt.attemptLeaseMillis as number,
    state: receipt.state,
    selectedPair: receipt.selectedPair ?? null,
    challengeBindingSha256: requireSha256(receipt.challengeBindingSha256),
    failureCode: receipt.failureCode,
  })
}

function exactRecord(value: unknown, keys: readonly string[]): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalidReceipt()
  const record = value as Record<string, unknown>
  const actual = Object.keys(record)
  if (actual.length !== keys.length || actual.some((key, index) => key !== keys[index])) {
    invalidReceipt()
  }
  return record
}

function requireSha256(value: unknown): string {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) invalidReceipt()
  return value
}

function requireSignature(value: unknown): string {
  if (typeof value !== 'string' || !SIGNATURE_PATTERN.test(value)) invalidReceipt()
  const bytes = Buffer.from(value, 'base64url')
  if (bytes.byteLength !== 64 || bytes.toString('base64url') !== value) invalidReceipt()
  return value
}

function requireTimestamp(value: unknown): string {
  if (typeof value !== 'string' || !UTC_TIMESTAMP_PATTERN.test(value)) invalidReceipt()
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) invalidReceipt()
  return value
}

function invalidReceipt(): never {
  throw new Error('external fixture terminal receipt is invalid')
}
