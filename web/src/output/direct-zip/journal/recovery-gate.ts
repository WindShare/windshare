import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalU8,
  canonicalU64,
  concatCanonicalBytes,
  snapshotIdentity,
  equalCanonicalBytes,
  type CanonicalBytes,
} from '../../workspace/canonical'
import { CanonicalRecordReader } from '../../workspace/canonical-reader'
import type { RecoveryGateKind } from '../../workspace/state'
import type { DirectZipRecoveryGateV1 } from './model'

export async function createDirectZipRecoveryGateV1(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly kind: RecoveryGateKind
  readonly checkpointDigest: string
  readonly candidateDigest?: string
  readonly additionalTemporaryBytesUpperBound?: bigint
}): Promise<DirectZipRecoveryGateV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const receiveIntentDigest = snapshotIdentity(
    input.receiveIntentDigest,
    32,
    'receive intent digest',
  )
  const checkpointDigest = snapshotIdentity(input.checkpointDigest, 32, 'checkpoint digest')
  const candidateDigest = input.candidateDigest === undefined
    ? undefined
    : snapshotIdentity(input.candidateDigest, 32, 'candidate digest')
  const additionalTemporaryBytesUpperBound = input.additionalTemporaryBytesUpperBound
  if ((input.kind === 'destination-space-required') !==
      (additionalTemporaryBytesUpperBound !== undefined)) {
    throw new TypeError('only a destination-space recovery gate owns a temporary-space bound')
  }
  if (additionalTemporaryBytesUpperBound !== undefined) {
    canonicalU64(additionalTemporaryBytesUpperBound)
  }
  const canonicalBytes = canonicalRecord('windshare/direct-zip-recovery-gate/v1', 1, [
    canonicalFrame(canonicalIdentity(operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalU8(recoveryGateKindByte(input.kind))),
    canonicalFrame(canonicalIdentity(checkpointDigest, 32, 'checkpoint digest')),
    optionalDigestFrame(candidateDigest),
    optionalU64Frame(additionalTemporaryBytesUpperBound),
  ])
  return Object.freeze({
    operationId,
    receiveIntentDigest,
    kind: input.kind,
    checkpointDigest,
    ...(candidateDigest === undefined ? {} : { candidateDigest }),
    ...(additionalTemporaryBytesUpperBound === undefined
      ? {}
      : { additionalTemporaryBytesUpperBound }),
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateDirectZipRecoveryGateV1(
  input: DirectZipRecoveryGateV1,
): Promise<DirectZipRecoveryGateV1> {
  const rebuilt = await createDirectZipRecoveryGateV1(input)
  if (rebuilt.digest !== input.digest ||
      !equalCanonicalBytes(rebuilt.canonicalBytes, input.canonicalBytes)) {
    throw new TypeError('Direct ZIP recovery gate changed its canonical authority')
  }
  return rebuilt
}

export async function decodeDirectZipRecoveryGateV1(
  canonicalBytes: Uint8Array,
): Promise<DirectZipRecoveryGateV1> {
  const reader = CanonicalRecordReader.open(
    canonicalBytes,
    'windshare/direct-zip-recovery-gate/v1',
    1,
  )
  const operationId = reader.framedIdentity(16, 'operation ID')
  const receiveIntentDigest = reader.framedIdentity(32, 'receive intent digest')
  const kind = recoveryGateKindFromByte(reader.frame())
  const checkpointDigest = reader.framedIdentity(32, 'checkpoint digest')
  const candidateDigest = optionalDigestValue(reader.frame())
  const additionalTemporaryBytesUpperBound = optionalU64Value(reader.frame())
  reader.finish('Direct ZIP recovery gate')
  return createDirectZipRecoveryGateV1({
    operationId,
    receiveIntentDigest,
    kind,
    checkpointDigest,
    ...(candidateDigest === undefined ? {} : { candidateDigest }),
    ...(additionalTemporaryBytesUpperBound === undefined
      ? {}
      : { additionalTemporaryBytesUpperBound }),
  })
}

function recoveryGateKindByte(kind: RecoveryGateKind): number {
  switch (kind) {
    case 'authorization-required': return 1
    case 'target-verification-required': return 2
    case 'destination-space-required': return 3
  }
}

function recoveryGateKindFromByte(bytes: Uint8Array): RecoveryGateKind {
  const reader = CanonicalRecordReader.value(bytes)
  const value = reader.byte('recovery gate kind')
  reader.finish('recovery gate kind')
  switch (value) {
    case 1: return 'authorization-required'
    case 2: return 'target-verification-required'
    case 3: return 'destination-space-required'
    default: throw new TypeError('Direct ZIP recovery gate kind is invalid')
  }
}

function optionalDigestFrame(value: string | undefined): CanonicalBytes {
  return canonicalFrame(value === undefined
    ? canonicalU8(1)
    : concatCanonicalBytes([
        canonicalU8(2),
        canonicalFrame(canonicalIdentity(value, 32, 'candidate digest')),
      ]))
}

function optionalU64Frame(value: bigint | undefined): CanonicalBytes {
  return canonicalFrame(value === undefined
    ? canonicalU8(1)
    : concatCanonicalBytes([canonicalU8(2), canonicalFrame(canonicalU64(value))]))
}

function optionalDigestValue(bytes: Uint8Array): string | undefined {
  const reader = CanonicalRecordReader.value(bytes)
  const tag = reader.byte('optional candidate digest')
  if (tag === 1) {
    reader.finish('optional candidate digest')
    return undefined
  }
  if (tag !== 2) throw new TypeError('optional candidate digest tag is invalid')
  const value = reader.framedIdentity(32, 'candidate digest')
  reader.finish('optional candidate digest')
  return value
}

function optionalU64Value(bytes: Uint8Array): bigint | undefined {
  const reader = CanonicalRecordReader.value(bytes)
  const tag = reader.byte('optional temporary-space bound')
  if (tag === 1) {
    reader.finish('optional temporary-space bound')
    return undefined
  }
  if (tag !== 2) throw new TypeError('optional temporary-space bound tag is invalid')
  const value = reader.framedU64('temporary-space bound')
  reader.finish('optional temporary-space bound')
  return value
}
