import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { encodeBase64Url } from '../../crypto/bytes'
import {
  checkpointConcatBytes,
  checkpointFramedBytes,
  checkpointSha256Sync,
  uint64Bytes,
} from './checkpoint-codec'
import { FileCheckpointError } from './checkpoint-lifecycle'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MAX_FILE_SIZE,
  FILE_ID_BYTES,
  FILE_REVISION_BYTES,
  OPERATION_ID_BYTES,
  checkpointMaterializerKind,
  identityBytes,
  type CheckpointIdentity,
  type FileCheckpointMaterializerKind,
} from './checkpoint-model'

export const CHECKPOINT_LINEAGE_DOMAIN = 'windshare/checkpoint-lineage/v1' as const

const TEXT_ENCODER = new TextEncoder()
const CHECKPOINT_LINEAGE_DOMAIN_BYTES = TEXT_ENCODER.encode(CHECKPOINT_LINEAGE_DOMAIN)

declare const checkpointLineageIdBrand: unique symbol
export type CheckpointLineageID = string & { readonly [checkpointLineageIdBrand]: true }

export interface CheckpointLineageSpec {
  readonly operationId: CheckpointIdentity
  readonly receiveIntentDigest: CheckpointIdentity
  readonly materializationBindingDigest: CheckpointIdentity
  readonly fileId: CheckpointIdentity
  readonly canonicalPath: readonly string[]
  readonly materializerKind: FileCheckpointMaterializerKind | number
  readonly authorityRef: CheckpointIdentity
}

interface NormalizedCheckpointLineageSpec {
  readonly operationId: Uint8Array<ArrayBuffer>
  readonly receiveIntentDigest: Uint8Array<ArrayBuffer>
  readonly materializationBindingDigest: Uint8Array<ArrayBuffer>
  readonly fileId: Uint8Array<ArrayBuffer>
  readonly canonicalPath: readonly string[]
  readonly materializerKind: FileCheckpointMaterializerKind
  readonly authorityRef: Uint8Array<ArrayBuffer>
}

export function canonicalCheckpointLineageBytes(
  input: CheckpointLineageSpec,
): Uint8Array<ArrayBuffer> {
  const spec = normalizeCheckpointLineageSpec(input)
  return checkpointConcatBytes([
    CHECKPOINT_LINEAGE_DOMAIN_BYTES,
    checkpointFramedBytes(spec.operationId),
    checkpointFramedBytes(spec.receiveIntentDigest),
    checkpointFramedBytes(spec.materializationBindingDigest),
    checkpointFramedBytes(spec.fileId),
    checkpointFramedBytes(canonicalCheckpointLineagePathBytes(spec.canonicalPath)),
    checkpointFramedBytes(Uint8Array.of(spec.materializerKind)),
    checkpointFramedBytes(spec.authorityRef),
  ])
}

export function deriveCheckpointLineageID(input: CheckpointLineageSpec): CheckpointLineageID {
  return encodeBase64Url(checkpointSha256Sync(
    canonicalCheckpointLineageBytes(input),
  )) as CheckpointLineageID
}

// Digest equality is only an index hit. Authority requires equality across all
// seven normalized coordinates so even a hypothetical SHA-256 collision cannot
// merge logical slots.
export function sameCheckpointLineageSpec(
  left: CheckpointLineageSpec,
  right: CheckpointLineageSpec,
): boolean {
  try {
    const leftSpec = normalizeCheckpointLineageSpec(left)
    const rightSpec = normalizeCheckpointLineageSpec(right)
    return sameBytes(leftSpec.operationId, rightSpec.operationId) &&
      sameBytes(leftSpec.receiveIntentDigest, rightSpec.receiveIntentDigest) &&
      sameBytes(leftSpec.materializationBindingDigest, rightSpec.materializationBindingDigest) &&
      sameBytes(leftSpec.fileId, rightSpec.fileId) &&
      samePath(leftSpec.canonicalPath, rightSpec.canonicalPath) &&
      leftSpec.materializerKind === rightSpec.materializerKind &&
      sameBytes(leftSpec.authorityRef, rightSpec.authorityRef)
  } catch {
    return false
  }
}

export type CheckpointLineageDecisionKind =
  | 'absent'
  | 'exact'
  | 'revision-conflict'
  | 'ownership-conflict'
  | 'invalid'

export interface CheckpointLineageRequest {
  readonly fileRevision: CheckpointIdentity
  readonly exactSize: bigint
}

export interface CheckpointLineageEvidence {
  readonly fileRevision: CheckpointIdentity
  readonly exactSize: bigint
  readonly ownedObjectId: CheckpointIdentity
}

// This is the only mixed-conflict precedence used by startup lookup and atomic
// creation: invalid same-revision binding, revision, ownership, exact, absent.
export function classifyCheckpointLineage(
  request: CheckpointLineageRequest,
  evidence: readonly CheckpointLineageEvidence[],
  crossLineageOwnershipConflict = false,
): CheckpointLineageDecisionKind {
  const normalizedRequest = normalizeCheckpointLineageRequest(request)
  if (normalizedRequest === undefined) return 'invalid'
  if (evidence.length === 0) return 'absent'

  const normalizedEvidence = evidence.map(normalizeCheckpointLineageEvidence)
  if (normalizedEvidence.some((record) => record === undefined)) return 'invalid'
  const records = normalizedEvidence as readonly NormalizedCheckpointLineageEvidence[]
  if (records.some((record) =>
    record.fileRevision === normalizedRequest.fileRevision &&
    record.exactSize !== normalizedRequest.exactSize)) return 'invalid'
  if (records.some((record) => record.fileRevision !== normalizedRequest.fileRevision)) {
    return 'revision-conflict'
  }
  if (crossLineageOwnershipConflict) return 'ownership-conflict'
  const firstObject = records[0]!.ownedObjectId
  if (records.some((record) => record.ownedObjectId !== firstObject)) return 'ownership-conflict'
  return 'exact'
}

function normalizeCheckpointLineageSpec(
  input: CheckpointLineageSpec,
): NormalizedCheckpointLineageSpec {
  let canonicalPath: readonly string[]
  try {
    canonicalPath = snapshotPortableCatalogPath(input.canonicalPath)
  } catch (cause) {
    throw new FileCheckpointError(
      'binding',
      cause instanceof Error
        ? `checkpoint lineage path is invalid: ${cause.message}`
        : 'checkpoint lineage path is invalid',
    )
  }
  return Object.freeze({
    operationId: identityBytes(input.operationId, OPERATION_ID_BYTES, 'operation ID'),
    receiveIntentDigest: identityBytes(
      input.receiveIntentDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'receive intent digest',
    ),
    materializationBindingDigest: identityBytes(
      input.materializationBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'materialization binding digest',
    ),
    fileId: identityBytes(input.fileId, FILE_ID_BYTES, 'file ID'),
    canonicalPath,
    materializerKind: checkpointMaterializerKind(input.materializerKind),
    authorityRef: identityBytes(
      input.authorityRef,
      FILE_CHECKPOINT_ID_BYTES,
      'authority reference',
    ),
  })
}

interface NormalizedCheckpointLineageRequest {
  readonly fileRevision: string
  readonly exactSize: bigint
}

interface NormalizedCheckpointLineageEvidence extends NormalizedCheckpointLineageRequest {
  readonly ownedObjectId: string
}

function normalizeCheckpointLineageRequest(
  request: CheckpointLineageRequest,
): NormalizedCheckpointLineageRequest | undefined {
  try {
    if (typeof request.exactSize !== 'bigint' || request.exactSize < 0n ||
        request.exactSize > FILE_CHECKPOINT_MAX_FILE_SIZE) return undefined
    return {
      fileRevision: encodeBase64Url(identityBytes(
        request.fileRevision,
        FILE_REVISION_BYTES,
        'file revision',
      )),
      exactSize: request.exactSize,
    }
  } catch {
    return undefined
  }
}

function normalizeCheckpointLineageEvidence(
  evidence: CheckpointLineageEvidence,
): NormalizedCheckpointLineageEvidence | undefined {
  const request = normalizeCheckpointLineageRequest(evidence)
  if (request === undefined) return undefined
  try {
    return {
      ...request,
      ownedObjectId: encodeBase64Url(identityBytes(
        evidence.ownedObjectId,
        FILE_CHECKPOINT_ID_BYTES,
        'owned object ID',
      )),
    }
  } catch {
    return undefined
  }
}

function canonicalCheckpointLineagePathBytes(
  path: readonly string[],
): Uint8Array<ArrayBuffer> {
  return checkpointConcatBytes([
    uint64Bytes(BigInt(path.length)),
    ...path.map((segment) => checkpointFramedBytes(TEXT_ENCODER.encode(segment))),
  ])
}

function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  return left.byteLength === right.byteLength && left.every((value, index) => value === right[index])
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}
