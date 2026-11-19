import { FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_PHASE_ACTIVE, FILE_CHECKPOINT_PHASE_PAUSED } from './checkpoint-lifecycle'
import { compareRevisionMetadata, sourceMetadataFields, type RevisionMetadataComparison } from '../../content/revision-comparison'
import { snapshotCatalogPath } from '../../catalog/path-policy'
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
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
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

export type CheckpointSourceRejection<Evidence extends CheckpointLineageEvidence> =
  | Readonly<{
      kind: 'revision-conflict'
      comparison: Extract<RevisionMetadataComparison, { kind: 'observed-change' }>
      previous: Evidence
    }>
  | Readonly<{
      kind: 'revision-evidence-unavailable'
      comparison: Extract<RevisionMetadataComparison, { kind: 'unavailable' }>
      previous: Evidence
    }>

type CheckpointLineageAssessment<Evidence extends CheckpointLineageEvidence> =
  | Readonly<{ kind: 'absent' | 'exact' | 'metadata-match' | 'reuse-state-blocked' | 'ownership-conflict' | 'invalid' }>
  | CheckpointSourceRejection<Evidence>

export interface CheckpointLineageRequest {
  readonly fileRevision: CheckpointIdentity
  readonly sourceMetadata?: string
  readonly exactSize: bigint
}

export interface CheckpointLineageEvidence {
  readonly fileRevision: CheckpointIdentity
  readonly sourceMetadata?: string
  readonly exactSize: bigint
  readonly ownedObjectId: CheckpointIdentity
  readonly phase?: number
  readonly commitState?: number
}

// Missing evidence can defer reuse only where checkpoint state and ownership allow it.
export function classifyCheckpointLineage<Evidence extends CheckpointLineageEvidence>(
  request: CheckpointLineageRequest,
  evidence: readonly Evidence[],
  crossLineageOwnershipConflict = false,
): CheckpointLineageAssessment<Evidence> {
  const current = normalizeCheckpointLineageRequest(request)
  if (current === undefined) return { kind: 'invalid' }
  if (evidence.length === 0) return { kind: 'absent' }

  let firstObject: string | undefined
  let ownershipConflict = crossLineageOwnershipConflict
  let differentRevision = false
  let reuseStateBlocked = false
  let changed: CheckpointSourceRejection<Evidence> | undefined
  let unavailable: CheckpointSourceRejection<Evidence> | undefined
  for (const previous of evidence) {
    const record = normalizeCheckpointLineageEvidence(previous)
    if (record === undefined) return { kind: 'invalid' }
    firstObject ??= record.ownedObjectId
    ownershipConflict ||= firstObject !== record.ownedObjectId
    const comparison = compareRevisionMetadata(record, current)
    const sameRevision = record.fileRevision === current.fileRevision
    differentRevision ||= !sameRevision
    switch (comparison.kind) {
      case 'observed-change':
        if (sameRevision) return { kind: 'invalid' }
        changed ??= { kind: 'revision-conflict', comparison, previous }
        break
      case 'unavailable':
        unavailable ??= { kind: 'revision-evidence-unavailable', comparison, previous }
        break
    }
    reuseStateBlocked ||= !sameRevision && !checkpointStatePermitsMetadataReuse(record)
  }

  if (changed !== undefined) return changed
  if (reuseStateBlocked) return { kind: 'reuse-state-blocked' }
  if (ownershipConflict) return { kind: 'ownership-conflict' }
  return unavailable ?? { kind: differentRevision ? 'metadata-match' : 'exact' }
}

function checkpointStatePermitsMetadataReuse(record: NormalizedCheckpointLineageEvidence): boolean {
  return record.commitState === FILE_CHECKPOINT_COMMIT_VERIFIED &&
    (record.phase === FILE_CHECKPOINT_PHASE_ACTIVE || record.phase === FILE_CHECKPOINT_PHASE_PAUSED)
}

function normalizeCheckpointLineageSpec(
  input: CheckpointLineageSpec,
): NormalizedCheckpointLineageSpec {
  const materializerKind = checkpointMaterializerKind(input.materializerKind)
  let canonicalPath: readonly string[]
  try {
    // Only the current FSA identity may encode the named root entry as [].
    canonicalPath = materializerKind === FILE_CHECKPOINT_MATERIALIZER_FSA_TREE &&
        Array.isArray(input.canonicalPath) && input.canonicalPath.length === 0
      ? Object.freeze([])
      : snapshotCatalogPath(input.canonicalPath)
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
    materializerKind,
    authorityRef: identityBytes(
      input.authorityRef,
      FILE_CHECKPOINT_ID_BYTES,
      'authority reference',
    ),
  })
}

interface NormalizedCheckpointLineageRequest {
  readonly fileRevision: string
  readonly sourceMetadata?: string
  readonly exactSize: bigint
}

interface NormalizedCheckpointLineageEvidence extends NormalizedCheckpointLineageRequest {
  readonly ownedObjectId: string
  readonly phase?: number
  readonly commitState?: number
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
      ...sourceMetadataFields(request),
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
      ...(evidence.phase === undefined ? {} : { phase: evidence.phase }),
      ...(evidence.commitState === undefined ? {} : { commitState: evidence.commitState }),
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
