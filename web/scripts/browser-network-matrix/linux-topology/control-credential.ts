import { createHash, createPublicKey, randomBytes, verify } from 'node:crypto'

import {
  type NetworkMatrixControlAuthority,
  type NetworkMatrixSampleAuthority,
  parseNetworkMatrixControlAuthority,
  sameNetworkMatrixControlAuthority,
  sameNetworkMatrixSampleAuthority,
} from '../sample-authority.ts'
import {
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
  EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
} from './external-fixture-attestation.ts'

const MINIMUM_CONTROL_CREDENTIAL_BYTES = 32
const MAXIMUM_CONTROL_CREDENTIAL_BYTES = 512
const LEASE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const CANONICAL_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const SIGNATURE_PATTERN = /^[A-Za-z0-9_-]{86}$/u
const UTC_TIMESTAMP_PATTERN =
  /^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u
const MAXIMUM_CONTROL_LEASE_MS = 120_000

export interface ExternalFixtureControlCredentialReceipt {
  readonly protocolVersion: typeof EXTERNAL_FIXTURE_CONTROL_PROTOCOL
  readonly operation: 'release' | 'revoke-and-wait'
  readonly requestId: string
  readonly releaseRequestId: string
  readonly revokeRequestId: string
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly probeNonce: string
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly leaseExpiresAt: string
  readonly controlTerminal: 'revoked'
  readonly turnProviderLeaseId: string
  readonly turnTerminal: 'revoked' | 'not-required'
  readonly terminal: 'revoked'
  readonly retiredAt: string
}

export interface ExternalFixtureControlCredentialLeasePayload {
  readonly protocolVersion: typeof EXTERNAL_FIXTURE_CONTROL_PROTOCOL
  readonly requestId: string
  readonly releaseRequestId: string
  readonly revokeRequestId: string
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly probeNonce: string
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly issuedAt: string
  readonly expiresAt: string
  readonly maxAttempts: 1
  readonly credentialByteLength: number
  readonly turnCapability: 'bound' | 'not-required'
  readonly turnProviderLeaseId: string
  readonly turnCredentialId: string
  readonly turnUsername: string
  readonly turnExpiresAt: string
}

export interface SignedExternalFixtureControlCredentialLease {
  readonly protocolVersion: typeof EXTERNAL_FIXTURE_CONTROL_PROTOCOL
  readonly lease: ExternalFixtureControlCredentialLeasePayload
  readonly leaseSha256: string
  readonly signatureAlgorithm: typeof EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM
  readonly signature: string
}

export interface SignedExternalFixtureControlCredentialReceipt {
  readonly protocolVersion: typeof EXTERNAL_FIXTURE_CONTROL_PROTOCOL
  readonly receipt: ExternalFixtureControlCredentialReceipt
  readonly receiptSha256: string
  readonly signatureAlgorithm: typeof EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM
  readonly signature: string
}

export interface ExternalFixtureControlCredentialRetirementReceipt {
  readonly receipt: ExternalFixtureControlCredentialReceipt
  readonly signedReceipt: SignedExternalFixtureControlCredentialReceipt
}

export interface ExternalFixtureControlCredentialAuthorityReceipt {
  readonly terminal: 'closed'
}

export interface ExternalFixtureControlCredentialLease {
  readonly signedLease: SignedExternalFixtureControlCredentialLease
  readonly requestId: string
  readonly releaseRequestId: string
  readonly revokeRequestId: string
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly probeNonce: string
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly issuedAt: string
  readonly expiresAt: string
  readonly maxAttempts: 1
  readonly turnCapability: 'bound' | 'not-required'
  readonly turnProviderLeaseId: string
  readonly turnCredentialId: string
  readonly turnUsername: string
  readonly turnExpiresAt: string
  /** Ownership transfers to the caller; the provider must not retain an alias to these bytes. */
  readonly credential: Uint8Array
  release(): Promise<ExternalFixtureControlCredentialRetirementReceipt>
  revokeAndWait(): Promise<ExternalFixtureControlCredentialRetirementReceipt>
}

export interface ExternalFixtureControlCredentialAuthority {
  acquire(input: {
    readonly sampleAuthority: NetworkMatrixSampleAuthority
    readonly probeNonce: string
    readonly signal: AbortSignal
  }): Promise<ExternalFixtureControlCredentialLease>
  closeAndWait(): Promise<ExternalFixtureControlCredentialAuthorityReceipt>
  forceTerminateAndWait(): Promise<ExternalFixtureControlCredentialAuthorityReceipt>
}

export interface ExternalFixtureControlCredentialRetirementOutcome {
  readonly receipt: ExternalFixtureControlCredentialReceipt
  readonly signedLease: SignedExternalFixtureControlCredentialLease
  readonly signedReceipt: SignedExternalFixtureControlCredentialReceipt
  readonly deliveryOutcome:
    | 'release-confirmed'
    | 'release-failed-revoke-confirmed'
    | 'forced-revoke-confirmed'
}

export function parseSignedExternalFixtureControlCredentialLease(
  value: unknown,
): SignedExternalFixtureControlCredentialLease {
  const envelope = exactRecord(value, [
    'protocolVersion', 'lease', 'leaseSha256', 'signatureAlgorithm', 'signature',
  ])
  if (
    envelope.protocolVersion !== EXTERNAL_FIXTURE_CONTROL_PROTOCOL ||
    envelope.signatureAlgorithm !== EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM
  ) invalidSignedAuthority()
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    lease: parseExternalFixtureControlCredentialLeasePayload(envelope.lease),
    leaseSha256: requireSha256(envelope.leaseSha256),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: requireSignature(envelope.signature),
  })
}

export function parseSignedExternalFixtureControlCredentialReceipt(
  value: unknown,
): SignedExternalFixtureControlCredentialReceipt {
  const envelope = exactRecord(value, [
    'protocolVersion', 'receipt', 'receiptSha256', 'signatureAlgorithm', 'signature',
  ])
  if (
    envelope.protocolVersion !== EXTERNAL_FIXTURE_CONTROL_PROTOCOL ||
    envelope.signatureAlgorithm !== EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM
  ) invalidSignedAuthority()
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    receipt: parseExternalFixtureControlCredentialReceipt(envelope.receipt),
    receiptSha256: requireSha256(envelope.receiptSha256),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: requireSignature(envelope.signature),
  })
}

export function authenticateSignedExternalFixtureControlCredentialLease(
  value: unknown,
  publicKey: string | Buffer,
): ExternalFixtureControlCredentialLeasePayload {
  const envelope = parseSignedExternalFixtureControlCredentialLease(value)
  authenticateEnvelope(
    canonicalExternalFixtureControlCredentialLeaseJson(envelope.lease),
    envelope.leaseSha256,
    envelope.signature,
    publicKey,
  )
  return envelope.lease
}

export function authenticateSignedExternalFixtureControlCredentialReceipt(
  value: unknown,
  publicKey: string | Buffer,
): ExternalFixtureControlCredentialReceipt {
  const envelope = parseSignedExternalFixtureControlCredentialReceipt(value)
  authenticateEnvelope(
    canonicalExternalFixtureControlCredentialReceiptJson(envelope.receipt),
    envelope.receiptSha256,
    envelope.signature,
    publicKey,
  )
  return envelope.receipt
}

export function canonicalExternalFixtureControlCredentialLeaseJson(
  value: ExternalFixtureControlCredentialLeasePayload,
): string {
  return `${JSON.stringify(parseExternalFixtureControlCredentialLeasePayload(value))}\n`
}

export function canonicalExternalFixtureControlCredentialReceiptJson(
  value: ExternalFixtureControlCredentialReceipt,
): string {
  return `${JSON.stringify(parseExternalFixtureControlCredentialReceipt(value))}\n`
}

export function validateExternalFixtureControlCredential(
  lease: ExternalFixtureControlCredentialLease,
  expected?: {
    readonly sampleAuthority: NetworkMatrixSampleAuthority
    readonly probeNonce: string
    readonly now?: number
  },
): void {
  const signedLease = parseSignedExternalFixtureControlCredentialLease(lease.signedLease)
  const payload = signedLease.lease
  if (
    typeof lease !== 'object' || lease === null ||
    !(lease.credential instanceof Uint8Array) || typeof lease.release !== 'function' ||
    typeof lease.revokeAndWait !== 'function' ||
    !LEASE_ID_PATTERN.test(lease.requestId) || !LEASE_ID_PATTERN.test(lease.releaseRequestId) ||
    !LEASE_ID_PATTERN.test(lease.revokeRequestId) ||
    lease.requestId === lease.releaseRequestId || lease.requestId === lease.revokeRequestId ||
    lease.releaseRequestId === lease.revokeRequestId ||
    !LEASE_ID_PATTERN.test(lease.probeNonce) ||
    !CANONICAL_ID_PATTERN.test(lease.authorityInstanceId) ||
    !SHA256_PATTERN.test(lease.attestationSha256) || lease.maxAttempts !== 1 ||
    (lease.controlAuthority.sampleAuthority.profileId === 'scheduled-coturn'
      ? lease.turnCapability !== 'bound' || !LEASE_ID_PATTERN.test(lease.turnProviderLeaseId) ||
        !CANONICAL_ID_PATTERN.test(lease.turnCredentialId) ||
        typeof lease.turnUsername !== 'string' || lease.turnUsername.length === 0 ||
        lease.turnExpiresAt !== lease.expiresAt
      : lease.turnCapability !== 'not-required' || lease.turnProviderLeaseId !== '' ||
        lease.turnCredentialId !== '' || lease.turnUsername !== '' || lease.turnExpiresAt !== '') ||
    !isControlCredentialBytes(lease.credential)
  ) throw new Error('external fixture control credential lease is invalid')
  if (
    payload.requestId !== lease.requestId ||
    payload.releaseRequestId !== lease.releaseRequestId ||
    payload.revokeRequestId !== lease.revokeRequestId ||
    !sameNetworkMatrixControlAuthority(payload.controlAuthority, lease.controlAuthority) ||
    payload.probeNonce !== lease.probeNonce ||
    payload.authorityInstanceId !== lease.authorityInstanceId ||
    payload.attestationSha256 !== lease.attestationSha256 || payload.issuedAt !== lease.issuedAt ||
    payload.expiresAt !== lease.expiresAt || payload.maxAttempts !== lease.maxAttempts ||
    payload.credentialByteLength !== lease.credential.byteLength ||
    payload.turnCapability !== lease.turnCapability ||
    payload.turnProviderLeaseId !== lease.turnProviderLeaseId ||
    payload.turnCredentialId !== lease.turnCredentialId ||
    payload.turnUsername !== lease.turnUsername || payload.turnExpiresAt !== lease.turnExpiresAt
  ) throw new Error('external fixture control credential lease projection is invalid')
  if (
    expected !== undefined &&
    (!sameNetworkMatrixSampleAuthority(
      lease.controlAuthority.sampleAuthority,
      expected.sampleAuthority,
    ) || lease.probeNonce !== expected.probeNonce)
  ) throw new Error('external fixture control credential lease scope is invalid')
  const issuedAt = canonicalTimestamp(lease.issuedAt)
  const expiresAt = canonicalTimestamp(lease.expiresAt)
  const now = expected?.now ?? Date.now()
  if (
    expiresAt <= now || issuedAt > now + 30_000 || expiresAt <= issuedAt ||
    expiresAt - issuedAt > MAXIMUM_CONTROL_LEASE_MS
  ) throw new Error('external fixture control credential lease lifetime is invalid')
}

export function newExternalFixtureProbeNonce(): string {
  return randomBytes(24).toString('base64url')
}

export async function retireExternalFixtureControlCredential(
  lease: ExternalFixtureControlCredentialLease,
  force = false,
): Promise<ExternalFixtureControlCredentialRetirementOutcome> {
  let overwriteFailure: unknown
  try {
    lease.credential.fill(0)
  } catch (cause) {
    overwriteFailure = cause
  }
  let releaseFailure: unknown
  let terminal: ExternalFixtureControlCredentialRetirementReceipt | undefined
  try {
    const receipt = force ? await lease.revokeAndWait() : await lease.release()
    terminal = requireReceipt(receipt, lease)
  } catch (cause) {
    releaseFailure = cause
  }
  let revokeFailure: unknown
  if (releaseFailure !== undefined && !force) {
    try {
      terminal = requireReceipt(await lease.revokeAndWait(), lease)
    } catch (cause) {
      revokeFailure = cause
    }
  }
  if (overwriteFailure !== undefined || terminal === undefined || revokeFailure !== undefined ||
      force && releaseFailure !== undefined) {
    throw new AggregateError(
      [overwriteFailure, releaseFailure, revokeFailure].filter((cause) => cause !== undefined),
      'external fixture control credential ownership did not retire',
    )
  }
  let deliveryOutcome: ExternalFixtureControlCredentialRetirementOutcome['deliveryOutcome']
  if (force) deliveryOutcome = 'forced-revoke-confirmed'
  else if (releaseFailure === undefined) deliveryOutcome = 'release-confirmed'
  else deliveryOutcome = 'release-failed-revoke-confirmed'
  return Object.freeze({
    receipt: terminal.receipt,
    signedLease: lease.signedLease,
    signedReceipt: terminal.signedReceipt,
    deliveryOutcome,
  })
}

export function eraseExternalFixtureControlCredential(
  lease: ExternalFixtureControlCredentialLease,
): void {
  lease.credential.fill(0)
}

function requireReceipt(
  value: unknown,
  lease: ExternalFixtureControlCredentialLease,
): ExternalFixtureControlCredentialRetirementReceipt {
  const wrapper = exactRecord(value, ['receipt', 'signedReceipt'])
  const receipt = parseExternalFixtureControlCredentialReceipt(wrapper.receipt)
  const signedReceipt = parseSignedExternalFixtureControlCredentialReceipt(wrapper.signedReceipt)
  if (
    canonicalExternalFixtureControlCredentialReceiptJson(signedReceipt.receipt) !==
      canonicalExternalFixtureControlCredentialReceiptJson(receipt) ||
    receipt.operation !== 'release' && receipt.operation !== 'revoke-and-wait' ||
    receipt.requestId !== (receipt.operation === 'release'
      ? lease.releaseRequestId
      : lease.revokeRequestId) ||
    receipt.releaseRequestId !== lease.releaseRequestId ||
    receipt.revokeRequestId !== lease.revokeRequestId ||
    !sameNetworkMatrixControlAuthority(receipt.controlAuthority, lease.controlAuthority) ||
    receipt.probeNonce !== lease.probeNonce ||
    receipt.authorityInstanceId !== lease.authorityInstanceId ||
    receipt.attestationSha256 !== lease.attestationSha256 ||
    receipt.leaseExpiresAt !== lease.expiresAt || receipt.controlTerminal !== 'revoked' ||
    (lease.controlAuthority.sampleAuthority.profileId === 'scheduled-coturn'
      ? !LEASE_ID_PATTERN.test(receipt.turnProviderLeaseId) ||
        receipt.turnTerminal !== 'revoked'
      : receipt.turnProviderLeaseId !== '' || receipt.turnTerminal !== 'not-required') ||
    receipt.terminal !== 'revoked' || canonicalTimestamp(receipt.retiredAt) < 0
  ) throw new Error('external fixture control credential revocation receipt is invalid')
  return Object.freeze({ receipt, signedReceipt })
}

export function parseExternalFixtureControlCredentialLeasePayload(
  value: unknown,
): ExternalFixtureControlCredentialLeasePayload {
  const lease = exactRecord(value, [
    'protocolVersion', 'requestId', 'releaseRequestId', 'revokeRequestId',
    'controlAuthority', 'probeNonce', 'authorityInstanceId', 'attestationSha256',
    'issuedAt', 'expiresAt', 'maxAttempts', 'credentialByteLength', 'turnCapability',
    'turnProviderLeaseId', 'turnCredentialId', 'turnUsername', 'turnExpiresAt',
  ])
  const controlAuthority = parseNetworkMatrixControlAuthority(lease.controlAuthority)
  if (
    lease.protocolVersion !== EXTERNAL_FIXTURE_CONTROL_PROTOCOL ||
    !LEASE_ID_PATTERN.test(lease.requestId as string) ||
    !LEASE_ID_PATTERN.test(lease.releaseRequestId as string) ||
    !LEASE_ID_PATTERN.test(lease.revokeRequestId as string) ||
    lease.requestId === lease.releaseRequestId || lease.requestId === lease.revokeRequestId ||
    lease.releaseRequestId === lease.revokeRequestId ||
    !LEASE_ID_PATTERN.test(lease.probeNonce as string) ||
    !CANONICAL_ID_PATTERN.test(lease.authorityInstanceId as string) ||
    !SHA256_PATTERN.test(lease.attestationSha256 as string) || lease.maxAttempts !== 1 ||
    !Number.isSafeInteger(lease.credentialByteLength) ||
    (lease.credentialByteLength as number) < MINIMUM_CONTROL_CREDENTIAL_BYTES ||
    (lease.credentialByteLength as number) > MAXIMUM_CONTROL_CREDENTIAL_BYTES
  ) invalidSignedAuthority()
  const issuedAt = requireTimestamp(lease.issuedAt)
  const expiresAt = requireTimestamp(lease.expiresAt)
  if (Date.parse(expiresAt) <= Date.parse(issuedAt)) invalidSignedAuthority()
  const coturn = controlAuthority.sampleAuthority.profileId === 'scheduled-coturn'
  if (
    coturn
      ? lease.turnCapability !== 'bound' ||
        !LEASE_ID_PATTERN.test(lease.turnProviderLeaseId as string) ||
        !CANONICAL_ID_PATTERN.test(lease.turnCredentialId as string) ||
        typeof lease.turnUsername !== 'string' || lease.turnUsername.length === 0 ||
        lease.turnExpiresAt !== expiresAt
      : lease.turnCapability !== 'not-required' || lease.turnProviderLeaseId !== '' ||
        lease.turnCredentialId !== '' || lease.turnUsername !== '' || lease.turnExpiresAt !== ''
  ) invalidSignedAuthority()
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    requestId: lease.requestId as string,
    releaseRequestId: lease.releaseRequestId as string,
    revokeRequestId: lease.revokeRequestId as string,
    controlAuthority,
    probeNonce: lease.probeNonce as string,
    authorityInstanceId: lease.authorityInstanceId as string,
    attestationSha256: lease.attestationSha256 as string,
    issuedAt,
    expiresAt,
    maxAttempts: 1,
    credentialByteLength: lease.credentialByteLength as number,
    turnCapability: lease.turnCapability as 'bound' | 'not-required',
    turnProviderLeaseId: lease.turnProviderLeaseId as string,
    turnCredentialId: lease.turnCredentialId as string,
    turnUsername: lease.turnUsername as string,
    turnExpiresAt: lease.turnExpiresAt as string,
  })
}

export function parseExternalFixtureControlCredentialReceipt(
  value: unknown,
): ExternalFixtureControlCredentialReceipt {
  const receipt = exactRecord(value, [
    'protocolVersion', 'operation', 'requestId', 'releaseRequestId', 'revokeRequestId',
    'controlAuthority', 'probeNonce', 'authorityInstanceId', 'attestationSha256',
    'leaseExpiresAt', 'controlTerminal', 'turnProviderLeaseId', 'turnTerminal',
    'terminal', 'retiredAt',
  ])
  const controlAuthority = parseNetworkMatrixControlAuthority(receipt.controlAuthority)
  if (
    receipt.protocolVersion !== EXTERNAL_FIXTURE_CONTROL_PROTOCOL ||
    receipt.operation !== 'release' && receipt.operation !== 'revoke-and-wait' ||
    !LEASE_ID_PATTERN.test(receipt.requestId as string) ||
    !LEASE_ID_PATTERN.test(receipt.releaseRequestId as string) ||
    !LEASE_ID_PATTERN.test(receipt.revokeRequestId as string) ||
    !LEASE_ID_PATTERN.test(receipt.probeNonce as string) ||
    !CANONICAL_ID_PATTERN.test(receipt.authorityInstanceId as string) ||
    !SHA256_PATTERN.test(receipt.attestationSha256 as string) ||
    receipt.controlTerminal !== 'revoked' || receipt.terminal !== 'revoked'
  ) invalidSignedAuthority()
  const coturn = controlAuthority.sampleAuthority.profileId === 'scheduled-coturn'
  if (
    coturn
      ? !LEASE_ID_PATTERN.test(receipt.turnProviderLeaseId as string) ||
        receipt.turnTerminal !== 'revoked'
      : receipt.turnProviderLeaseId !== '' || receipt.turnTerminal !== 'not-required'
  ) invalidSignedAuthority()
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    operation: receipt.operation,
    requestId: receipt.requestId as string,
    releaseRequestId: receipt.releaseRequestId as string,
    revokeRequestId: receipt.revokeRequestId as string,
    controlAuthority,
    probeNonce: receipt.probeNonce as string,
    authorityInstanceId: receipt.authorityInstanceId as string,
    attestationSha256: receipt.attestationSha256 as string,
    leaseExpiresAt: requireTimestamp(receipt.leaseExpiresAt),
    controlTerminal: 'revoked',
    turnProviderLeaseId: receipt.turnProviderLeaseId as string,
    turnTerminal: receipt.turnTerminal as 'revoked' | 'not-required',
    terminal: 'revoked',
    retiredAt: requireTimestamp(receipt.retiredAt),
  })
}

function canonicalTimestamp(value: string): number {
  if (typeof value !== 'string' || !UTC_TIMESTAMP_PATTERN.test(value)) {
    throw new Error('external fixture control credential lease timestamp is invalid')
  }
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) {
    throw new Error('external fixture control credential lease timestamp is invalid')
  }
  return timestamp
}

function requireTimestamp(value: unknown): string {
  if (typeof value !== 'string') invalidSignedAuthority()
  canonicalTimestamp(value)
  return value
}

function requireSha256(value: unknown): string {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) invalidSignedAuthority()
  return value
}

function requireSignature(value: unknown): string {
  if (typeof value !== 'string' || !SIGNATURE_PATTERN.test(value)) invalidSignedAuthority()
  const bytes = Buffer.from(value, 'base64url')
  if (bytes.byteLength !== 64 || bytes.toString('base64url') !== value) invalidSignedAuthority()
  return value
}

function authenticateEnvelope(
  canonical: string,
  expectedSha256: string,
  signature: string,
  publicKeyValue: string | Buffer,
): void {
  if (createHash('sha256').update(canonical).digest('hex') !== expectedSha256) {
    invalidSignedAuthority()
  }
  let publicKey: ReturnType<typeof createPublicKey>
  try {
    publicKey = createPublicKey(publicKeyValue)
  } catch {
    invalidSignedAuthority()
  }
  if (
    publicKey.asymmetricKeyType !== 'ed25519' ||
    !verify(null, Buffer.from(canonical), publicKey, Buffer.from(signature, 'base64url'))
  ) invalidSignedAuthority()
}

function exactRecord(value: unknown, expectedKeys: readonly string[]): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalidSignedAuthority()
  const prototype = Object.getPrototypeOf(value) as unknown
  if (prototype !== Object.prototype && prototype !== null) invalidSignedAuthority()
  const keys = Reflect.ownKeys(value)
  if (
    keys.length !== expectedKeys.length ||
    keys.some((key, index) => typeof key !== 'string' || key !== expectedKeys[index])
  ) invalidSignedAuthority()
  const descriptors = Object.getOwnPropertyDescriptors(value)
  if (expectedKeys.some((key) => {
    const descriptor = descriptors[key]
    return descriptor === undefined || !descriptor.enumerable || !('value' in descriptor)
  })) invalidSignedAuthority()
  return value as Record<string, unknown>
}

function invalidSignedAuthority(): never {
  throw new Error('external fixture signed control credential authority is invalid')
}

function isControlCredentialBytes(value: Uint8Array): boolean {
  if (
    value.byteLength < MINIMUM_CONTROL_CREDENTIAL_BYTES ||
    value.byteLength > MAXIMUM_CONTROL_CREDENTIAL_BYTES
  ) return false
  for (const byte of value) {
    const alphaNumeric = byte >= 0x30 && byte <= 0x39 ||
      byte >= 0x41 && byte <= 0x5a || byte >= 0x61 && byte <= 0x7a
    if (!alphaNumeric && byte !== 0x2d && byte !== 0x5f) return false
  }
  return true
}
