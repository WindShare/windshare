import type { NetworkMatrixProfileId } from '../../vocabulary.ts'
import { parseNetworkMatrixControlAuthority } from '../../sample-authority.ts'
import type { ManualOperatorTopologyIdentity } from '../external-fixture-attestation.ts'
import {
  REMOTE_PION_ATTESTATION_LEASE_MS,
  REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS,
  type RemotePionControlLeaseBinding,
} from '../remote-pion.ts'
import {
  CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA,
  type ContainedBrowserSampleSecret,
  type ContainedBrowserSampleSecretFrame,
} from './contracts.ts'
import {
  MAXIMUM_CONTAINED_BROWSER_SECRET_BYTES,
  OPAQUE_ID_PATTERN,
  PROCESS_INSTANCE_PATTERN,
  SHA256_PATTERN,
  canonicalTimestamp,
  exactRecord,
  invalidSecret,
  isControlCredentialBytes,
  requireCanonicalId,
} from './contract-validation.ts'

const SECRET_METADATA_LENGTH_BYTES = 4
const MAXIMUM_DURATION_MS = 300_000

export async function loadContainedBrowserSampleSecret(
  input: AsyncIterable<Uint8Array | string>,
): Promise<ContainedBrowserSampleSecret> {
  const chunks: Buffer[] = []
  let observedBytes = 0
  try {
    for await (const chunk of input) {
      const bytes = typeof chunk === 'string' ? Buffer.from(chunk, 'utf8') : Buffer.from(chunk)
      observedBytes += bytes.byteLength
      if (observedBytes > MAXIMUM_CONTAINED_BROWSER_SECRET_BYTES) invalidSecret()
      chunks.push(bytes)
    }
  } finally {
    const destroy = (input as { readonly destroy?: () => void }).destroy
    destroy?.call(input)
  }
  if (observedBytes === 0) invalidSecret()
  const bytes = Buffer.concat(chunks, observedBytes)
  try {
    return parseContainedBrowserSampleSecretFrame(bytes)
  } catch {
    return invalidSecret()
  } finally {
    bytes.fill(0)
    for (const chunk of chunks) chunk.fill(0)
  }
}

/**
 * The parent serializes only non-secret metadata as JSON. Credential ownership
 * remains an erasable byte range appended to the anonymous stdin frame.
 */
export function encodeContainedBrowserSampleSecretFrame(
  value: unknown,
  credential: Uint8Array,
): ContainedBrowserSampleSecretFrame {
  if (!isControlCredentialBytes(credential)) invalidSecret()
  const secret = parseContainedBrowserSampleSecret(withCredential(value, credential))
  const metadata = secretFrameMetadata(secret)
  const metadataBytes = Buffer.from(`${JSON.stringify(metadata)}\n`, 'utf8')
  if (
    metadataBytes.byteLength === 0 ||
    metadataBytes.byteLength + credential.byteLength + SECRET_METADATA_LENGTH_BYTES >
      MAXIMUM_CONTAINED_BROWSER_SECRET_BYTES
  ) invalidSecret()
  const credentialOffset = SECRET_METADATA_LENGTH_BYTES + metadataBytes.byteLength
  const frame = Buffer.alloc(credentialOffset + credential.byteLength)
  frame.writeUInt32BE(metadataBytes.byteLength, 0)
  metadataBytes.copy(frame, SECRET_METADATA_LENGTH_BYTES)
  frame.set(credential, credentialOffset)
  return Object.freeze({
    bytes: frame,
    credentialOffset,
    credentialByteLength: credential.byteLength,
  })
}

function parseContainedBrowserSampleSecretFrame(bytes: Buffer): ContainedBrowserSampleSecret {
  if (bytes.byteLength <= SECRET_METADATA_LENGTH_BYTES) invalidSecret()
  const metadataByteLength = bytes.readUInt32BE(0)
  const credentialOffset = SECRET_METADATA_LENGTH_BYTES + metadataByteLength
  if (
    metadataByteLength === 0 || credentialOffset >= bytes.byteLength ||
    credentialOffset > MAXIMUM_CONTAINED_BROWSER_SECRET_BYTES
  ) invalidSecret()
  const encodedMetadata = new TextDecoder('utf-8', { fatal: true }).decode(
    bytes.subarray(SECRET_METADATA_LENGTH_BYTES, credentialOffset),
  )
  if (
    !encodedMetadata.endsWith('\n') || encodedMetadata.includes('\r') ||
    encodedMetadata.slice(0, -1).includes('\n')
  ) invalidSecret()
  let metadata: unknown
  try {
    metadata = JSON.parse(encodedMetadata)
  } catch {
    invalidSecret()
  }
  if (encodedMetadata !== `${JSON.stringify(metadata)}\n`) invalidSecret()
  const credential = Buffer.from(bytes.subarray(credentialOffset))
  try {
    return parseContainedBrowserSampleSecret(secretFromFrameMetadata(metadata, credential))
  } catch {
    credential.fill(0)
    invalidSecret()
  }
}

function withCredential(value: unknown, credential: Uint8Array): unknown {
  const secret = exactRecord(value, [
    'schemaVersion', 'expectedConnectivity', 'control', 'attemptLeaseMs',
    'resultPollIntervalMs', 'resultDeadlineMs', 'challengeDeadlineMs', 'cleanupDeadlineMs',
  ])
  const control = exactRecord(secret.control, [
    'controllerOrigin', 'controlLease',
    'tlsCertificateAuthority', 'tlsCertificateSha256', 'attestationPublicKey',
    'manualOperatorIdentity',
  ])
  return {
    schemaVersion: secret.schemaVersion,
    expectedConnectivity: secret.expectedConnectivity,
    control: {
      controllerOrigin: control.controllerOrigin,
      controlLease: control.controlLease,
      tlsCertificateAuthority: control.tlsCertificateAuthority,
      tlsCertificateSha256: control.tlsCertificateSha256,
      attestationPublicKey: control.attestationPublicKey,
      credential,
      manualOperatorIdentity: control.manualOperatorIdentity,
    },
    attemptLeaseMs: secret.attemptLeaseMs,
    resultPollIntervalMs: secret.resultPollIntervalMs,
    resultDeadlineMs: secret.resultDeadlineMs,
    challengeDeadlineMs: secret.challengeDeadlineMs,
    cleanupDeadlineMs: secret.cleanupDeadlineMs,
  }
}

function secretFrameMetadata(secret: ContainedBrowserSampleSecret): unknown {
  return {
    schemaVersion: secret.schemaVersion,
    expectedConnectivity: secret.expectedConnectivity,
    control: {
      controllerOrigin: secret.control.controllerOrigin,
      controlLease: secret.control.controlLease,
      tlsCertificateAuthority: secret.control.tlsCertificateAuthority,
      tlsCertificateSha256: secret.control.tlsCertificateSha256,
      attestationPublicKey: secret.control.attestationPublicKey,
      credentialByteLength: secret.control.credential.byteLength,
      manualOperatorIdentity: secret.control.manualOperatorIdentity,
    },
    attemptLeaseMs: secret.attemptLeaseMs,
    resultPollIntervalMs: secret.resultPollIntervalMs,
    resultDeadlineMs: secret.resultDeadlineMs,
    challengeDeadlineMs: secret.challengeDeadlineMs,
    cleanupDeadlineMs: secret.cleanupDeadlineMs,
  }
}

function secretFromFrameMetadata(metadataValue: unknown, credential: Uint8Array): unknown {
  const metadata = exactRecord(metadataValue, [
    'schemaVersion', 'expectedConnectivity', 'control', 'attemptLeaseMs',
    'resultPollIntervalMs', 'resultDeadlineMs', 'challengeDeadlineMs', 'cleanupDeadlineMs',
  ])
  const control = exactRecord(metadata.control, [
    'controllerOrigin', 'controlLease',
    'tlsCertificateAuthority', 'tlsCertificateSha256', 'attestationPublicKey',
    'credentialByteLength', 'manualOperatorIdentity',
  ])
  if (control.credentialByteLength !== credential.byteLength) invalidSecret()
  return withCredential({
    schemaVersion: metadata.schemaVersion,
    expectedConnectivity: metadata.expectedConnectivity,
    control: {
      controllerOrigin: control.controllerOrigin,
      controlLease: control.controlLease,
      tlsCertificateAuthority: control.tlsCertificateAuthority,
      tlsCertificateSha256: control.tlsCertificateSha256,
      attestationPublicKey: control.attestationPublicKey,
      manualOperatorIdentity: control.manualOperatorIdentity,
    },
    attemptLeaseMs: metadata.attemptLeaseMs,
    resultPollIntervalMs: metadata.resultPollIntervalMs,
    resultDeadlineMs: metadata.resultDeadlineMs,
    challengeDeadlineMs: metadata.challengeDeadlineMs,
    cleanupDeadlineMs: metadata.cleanupDeadlineMs,
  }, credential)
}

export function parseContainedBrowserSampleSecret(value: unknown): ContainedBrowserSampleSecret {
  const secret = exactRecord(value, [
    'schemaVersion', 'expectedConnectivity', 'control', 'attemptLeaseMs',
    'resultPollIntervalMs', 'resultDeadlineMs', 'challengeDeadlineMs', 'cleanupDeadlineMs',
  ])
  if (secret.schemaVersion !== CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA) invalidSecret()
  const control = exactRecord(secret.control, [
    'controllerOrigin', 'controlLease',
    'tlsCertificateAuthority', 'tlsCertificateSha256', 'attestationPublicKey',
    'credential', 'manualOperatorIdentity',
  ])
  const controllerOrigin = requireOriginOnlyHttps(control.controllerOrigin)
  const controlLease = parseControlLease(control.controlLease)
  const sampleAuthority = controlLease.controlAuthority.sampleAuthority
  const expectedConnectivity = sampleAuthority.profileId === 'scheduled-restricted-udp'
    ? 'blocked'
    : 'established'
  if (
    secret.expectedConnectivity !== expectedConnectivity ||
    typeof control.tlsCertificateAuthority !== 'string' ||
    control.tlsCertificateAuthority.length === 0 ||
    control.tlsCertificateAuthority.length > MAXIMUM_CONTAINED_BROWSER_SECRET_BYTES ||
    typeof control.tlsCertificateSha256 !== 'string' ||
    !SHA256_PATTERN.test(control.tlsCertificateSha256) ||
    typeof control.attestationPublicKey !== 'string' ||
    control.attestationPublicKey.length === 0 ||
    control.attestationPublicKey.length > MAXIMUM_CONTAINED_BROWSER_SECRET_BYTES ||
    !isControlCredentialBytes(control.credential)
  ) invalidSecret()
  const manualOperatorIdentity = parseManualOperatorIdentity(
    control.manualOperatorIdentity,
    sampleAuthority.profileId,
  )
  const attemptLeaseMs = boundedMilliseconds(secret.attemptLeaseMs)
  const resultPollIntervalMs = boundedMilliseconds(secret.resultPollIntervalMs)
  const resultDeadlineMs = boundedMilliseconds(secret.resultDeadlineMs)
  const challengeDeadlineMs = boundedMilliseconds(secret.challengeDeadlineMs)
  const cleanupDeadlineMs = boundedMilliseconds(secret.cleanupDeadlineMs)
  if (
    resultPollIntervalMs >= resultDeadlineMs || resultDeadlineMs > attemptLeaseMs ||
    challengeDeadlineMs > attemptLeaseMs ||
    attemptLeaseMs > REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS ||
    attemptLeaseMs >= REMOTE_PION_ATTESTATION_LEASE_MS
  ) invalidSecret()
  return Object.freeze({
    schemaVersion: CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA,
    expectedConnectivity,
    control: Object.freeze({
      controllerOrigin,
      controlLease,
      tlsCertificateAuthority: control.tlsCertificateAuthority as string,
      tlsCertificateSha256: control.tlsCertificateSha256 as string,
      attestationPublicKey: control.attestationPublicKey as string,
      credential: control.credential,
      manualOperatorIdentity,
    }),
    attemptLeaseMs,
    resultPollIntervalMs,
    resultDeadlineMs,
    challengeDeadlineMs,
    cleanupDeadlineMs,
  })
}

function requireOriginOnlyHttps(value: unknown): string {
  if (typeof value !== 'string') invalidSecret()
  let endpoint: URL
  try {
    endpoint = new URL(value)
  } catch {
    invalidSecret()
  }
  if (
    endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
    endpoint.pathname !== '/' || endpoint.search !== '' || endpoint.hash !== '' ||
    value !== `${endpoint.origin}/`
  ) invalidSecret()
  return value
}

function boundedMilliseconds(value: unknown): number {
  if (
    !Number.isSafeInteger(value) || (value as number) < 1 ||
    (value as number) > MAXIMUM_DURATION_MS
  ) invalidSecret()
  return value as number
}

function parseManualOperatorIdentity(
  value: unknown,
  profileId: NetworkMatrixProfileId,
): ManualOperatorTopologyIdentity | null {
  if (profileId !== 'manual-real-nat') {
    if (value !== null) invalidSecret()
    return null
  }
  const identity = exactRecord(value, ['senderHostId', 'senderNetworkBoundaryId'])
  return Object.freeze({
    senderHostId: requireCanonicalId(identity.senderHostId),
    senderNetworkBoundaryId: requireCanonicalId(identity.senderNetworkBoundaryId),
  })
}

function parseControlLease(value: unknown): RemotePionControlLeaseBinding {
  const lease = exactRecord(value, [
    'controlAuthority', 'probeNonce', 'authorityInstanceId', 'attestationSha256',
    'issuedAt', 'expiresAt', 'maxAttempts',
  ])
  const controlAuthority = parseNetworkMatrixControlAuthority(lease.controlAuthority)
  if (
    typeof lease.probeNonce !== 'string' || !OPAQUE_ID_PATTERN.test(lease.probeNonce) ||
    typeof lease.authorityInstanceId !== 'string' ||
    !PROCESS_INSTANCE_PATTERN.test(lease.authorityInstanceId) ||
    typeof lease.attestationSha256 !== 'string' ||
    !SHA256_PATTERN.test(lease.attestationSha256) || lease.maxAttempts !== 1
  ) invalidSecret()
  const issuedAt = canonicalTimestamp(lease.issuedAt)
  const expiresAt = canonicalTimestamp(lease.expiresAt)
  if (expiresAt <= issuedAt || expiresAt - issuedAt > REMOTE_PION_ATTESTATION_LEASE_MS) {
    invalidSecret()
  }
  return Object.freeze({
    controlAuthority,
    probeNonce: lease.probeNonce,
    authorityInstanceId: lease.authorityInstanceId,
    attestationSha256: lease.attestationSha256,
    issuedAt: lease.issuedAt as string,
    expiresAt: lease.expiresAt as string,
    maxAttempts: 1,
  })
}
