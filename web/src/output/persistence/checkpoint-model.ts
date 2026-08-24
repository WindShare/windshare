import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_PUBLISHED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MAX_QUARANTINE_ORIGIN,
  FILE_CHECKPOINT_MAX_QUARANTINE_REASON,
  FILE_CHECKPOINT_MAX_RETIREMENT_REASON,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PUBLISHED,
  FileCheckpointError,
  checkpointCommitState,
  checkpointLifecycleClaim,
  checkpointPhase,
  validCheckpointLifecycleTransition,
  validateCheckpointLifecycle,
  type FileCheckpointCommitState,
  type FileCheckpointPhase,
  type FileCheckpointQuarantineOrigin,
  type FileCheckpointQuarantineReason,
  type FileCheckpointRetirementReason,
} from './checkpoint-lifecycle'

export const FILE_CHECKPOINT_V2_SCHEMA_VERSION = 2 as const
export const FILE_CHECKPOINT_OWNERSHIP_MARKER = 'windshare/file-checkpoint/v2' as const
export const FILE_CHECKPOINT_NAMESPACE = '.windshare-output/checkpoints-v2' as const

export const FILE_CHECKPOINT_MAX_RANGES = 16_384
export const MAX_CHECKPOINT_RECORDS_PER_OPERATION = 1_048_576
export const MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION = 1_048_576
export const CHECKPOINT_SHARD_BUCKETS = 256
export const FILE_CHECKPOINT_ID_BYTES = 32
export const OPERATION_ID_BYTES = 16
export const FILE_ID_BYTES = 16
export const FILE_REVISION_BYTES = 16
export const FILE_CHECKPOINT_MAX_FILE_SIZE = 0xffff_ffff_ffff_ffffn

export const FILE_CHECKPOINT_MATERIALIZER_NATIVE_TREE = 1 as const
export const FILE_CHECKPOINT_MATERIALIZER_LEGACY_FSA_TREE = 2 as const
export const FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE = 3 as const
export const FILE_CHECKPOINT_MATERIALIZER_ATOMIC_FILE = 4 as const
/** A new identity prevents legacy logical-root paths from authorizing reserved-root-relative FSA output. */
export const FILE_CHECKPOINT_MATERIALIZER_FSA_TREE = 5 as const

export type FileCheckpointMaterializerKind =
  | typeof FILE_CHECKPOINT_MATERIALIZER_NATIVE_TREE
  | typeof FILE_CHECKPOINT_MATERIALIZER_LEGACY_FSA_TREE
  | typeof FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE
  | typeof FILE_CHECKPOINT_MATERIALIZER_ATOMIC_FILE
  | typeof FILE_CHECKPOINT_MATERIALIZER_FSA_TREE

export interface FileCheckpointRange {
  readonly start: bigint
  readonly end: bigint
}

export type CheckpointRange = FileCheckpointRange
export type CheckpointIdentity = string | Uint8Array

export interface FileCheckpointSpec {
  readonly ownershipMarker?: string
  readonly namespace?: string
  readonly operationId: CheckpointIdentity
  readonly receiveIntentDigest: CheckpointIdentity
  readonly materializationBindingDigest: CheckpointIdentity
  readonly fileId: CheckpointIdentity
  readonly fileRevision: CheckpointIdentity
  readonly canonicalPath: readonly string[]
  readonly exactSize: bigint
  readonly materializerKind: FileCheckpointMaterializerKind | number
  readonly authorityRef: CheckpointIdentity
  readonly ownedObjectId: CheckpointIdentity
  readonly stateGeneration: bigint
  readonly checkpointGeneration: bigint
  readonly verifiedRanges: readonly FileCheckpointRange[]
  readonly phase?: FileCheckpointPhase | number
  readonly commitState?: FileCheckpointCommitState | number
  readonly quarantineReason?: FileCheckpointQuarantineReason | number
  readonly quarantineOrigin?: FileCheckpointQuarantineOrigin | number
  readonly retirementReason?: FileCheckpointRetirementReason | number
}

export interface FileCheckpointV2 {
  readonly schemaVersion: typeof FILE_CHECKPOINT_V2_SCHEMA_VERSION
  readonly ownershipMarker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespace: typeof FILE_CHECKPOINT_NAMESPACE
  readonly recordId: string
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
  readonly fileId: string
  readonly fileRevision: string
  readonly canonicalPath: readonly string[]
  readonly exactSize: bigint
  readonly materializerKind: FileCheckpointMaterializerKind
  readonly authorityRef: string
  readonly ownedObjectId: string
  readonly stateGeneration: bigint
  readonly checkpointGeneration: bigint
  readonly verifiedRanges: readonly FileCheckpointRange[]
  readonly phase: FileCheckpointPhase
  readonly commitState: FileCheckpointCommitState
  readonly quarantineReason: FileCheckpointQuarantineReason | 0
  readonly quarantineOrigin: FileCheckpointQuarantineOrigin | 0
  readonly retirementReason: FileCheckpointRetirementReason | 0
  readonly checksum: string
}

export interface NormalizedFileCheckpointSpec {
  readonly ownershipMarker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespace: typeof FILE_CHECKPOINT_NAMESPACE
  readonly operationId: Uint8Array<ArrayBuffer>
  readonly receiveIntentDigest: Uint8Array<ArrayBuffer>
  readonly materializationBindingDigest: Uint8Array<ArrayBuffer>
  readonly fileId: Uint8Array<ArrayBuffer>
  readonly fileRevision: Uint8Array<ArrayBuffer>
  readonly canonicalPath: readonly string[]
  readonly exactSize: bigint
  readonly materializerKind: FileCheckpointMaterializerKind
  readonly authorityRef: Uint8Array<ArrayBuffer>
  readonly ownedObjectId: Uint8Array<ArrayBuffer>
  readonly stateGeneration: bigint
  readonly checkpointGeneration: bigint
  readonly verifiedRanges: readonly FileCheckpointRange[]
  readonly phase: FileCheckpointPhase
  readonly commitState: FileCheckpointCommitState
  readonly quarantineReason: FileCheckpointQuarantineReason | 0
  readonly quarantineOrigin: FileCheckpointQuarantineOrigin | 0
  readonly retirementReason: FileCheckpointRetirementReason | 0
}

export function normalizeFileCheckpointSpec(
  spec: FileCheckpointSpec,
): NormalizedFileCheckpointSpec {
  const ownershipMarker = spec.ownershipMarker ?? FILE_CHECKPOINT_OWNERSHIP_MARKER
  const namespace = spec.namespace ?? FILE_CHECKPOINT_NAMESPACE
  if (ownershipMarker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      namespace !== FILE_CHECKPOINT_NAMESPACE) {
    throw new FileCheckpointError('ownership', 'FileCheckpointV2 ownership is invalid')
  }
  const exactSize = uint64(spec.exactSize, 'exact size')
  const stateGeneration = nonZeroUint64(spec.stateGeneration, 'state generation')
  const checkpointGeneration = uint64(spec.checkpointGeneration, 'checkpoint generation')
  const phase = checkpointPhase(spec.phase ?? FILE_CHECKPOINT_PHASE_ACTIVE)
  const commitState = checkpointCommitState(spec.commitState ?? FILE_CHECKPOINT_COMMIT_CANDIDATE)
  const quarantineReason = checkpointLifecycleClaim<FileCheckpointQuarantineReason>(
    spec.quarantineReason ?? 0,
    FILE_CHECKPOINT_MAX_QUARANTINE_REASON,
    'quarantine reason',
  )
  const quarantineOrigin = checkpointLifecycleClaim<FileCheckpointQuarantineOrigin>(
    spec.quarantineOrigin ?? 0,
    FILE_CHECKPOINT_MAX_QUARANTINE_ORIGIN,
    'quarantine origin',
  )
  const retirementReason = checkpointLifecycleClaim<FileCheckpointRetirementReason>(
    spec.retirementReason ?? 0,
    FILE_CHECKPOINT_MAX_RETIREMENT_REASON,
    'retirement reason',
  )
  validateCheckpointLifecycle(
    phase,
    commitState,
    quarantineReason,
    quarantineOrigin,
    retirementReason,
  )
  const materializerKind = checkpointMaterializerKind(spec.materializerKind)
  return Object.freeze({
    ownershipMarker,
    namespace,
    operationId: identityBytes(spec.operationId, OPERATION_ID_BYTES, 'operation ID'),
    receiveIntentDigest: identityBytes(
      spec.receiveIntentDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'receive intent digest',
    ),
    materializationBindingDigest: identityBytes(
      spec.materializationBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'materialization binding digest',
    ),
    fileId: identityBytes(spec.fileId, FILE_ID_BYTES, 'file ID'),
    fileRevision: identityBytes(spec.fileRevision, FILE_REVISION_BYTES, 'file revision'),
    canonicalPath: snapshotCheckpointPath(spec.canonicalPath, materializerKind),
    exactSize,
    materializerKind,
    authorityRef: identityBytes(spec.authorityRef, FILE_CHECKPOINT_ID_BYTES, 'authority reference'),
    ownedObjectId: identityBytes(spec.ownedObjectId, FILE_CHECKPOINT_ID_BYTES, 'owned object ID'),
    stateGeneration,
    checkpointGeneration,
    verifiedRanges: validateRanges(spec.verifiedRanges, exactSize),
    phase,
    commitState,
    quarantineReason,
    quarantineOrigin,
    retirementReason,
  })
}

export function checkpointMaterializerKind(value: number): FileCheckpointMaterializerKind {
  if (Number.isInteger(value) &&
      value >= FILE_CHECKPOINT_MATERIALIZER_NATIVE_TREE &&
      value <= FILE_CHECKPOINT_MATERIALIZER_FSA_TREE) {
    return value as FileCheckpointMaterializerKind
  }
  throw new FileCheckpointError('binding', 'checkpoint materializer kind is invalid')
}

export function checkpointIdentityEqual(
  left: FileCheckpointV2,
  right: FileCheckpointV2,
): boolean {
  return left.recordId === right.recordId &&
    left.operationId === right.operationId &&
    left.receiveIntentDigest === right.receiveIntentDigest &&
    left.materializationBindingDigest === right.materializationBindingDigest &&
    left.fileId === right.fileId && left.fileRevision === right.fileRevision &&
    samePath(left.canonicalPath, right.canonicalPath) && left.exactSize === right.exactSize &&
    left.materializerKind === right.materializerKind && left.authorityRef === right.authorityRef &&
    left.ownedObjectId === right.ownedObjectId
}

export function validateCheckpointTransitionSemantics(
  previous: FileCheckpointV2,
  next: FileCheckpointV2,
): void {
  if (!checkpointIdentityEqual(previous, next)) {
    throw new FileCheckpointError('binding', 'checkpoint immutable identity changed')
  }
  if (previous.phase === FILE_CHECKPOINT_PHASE_PUBLISHED ||
      previous.commitState === FILE_CHECKPOINT_COMMIT_PUBLISHED) {
    throw new FileCheckpointError('generation', 'published checkpoint is immutable')
  }
  if (next.stateGeneration < previous.stateGeneration ||
      next.checkpointGeneration < previous.checkpointGeneration) {
    throw new FileCheckpointError('generation', 'checkpoint generation regressed')
  }
  if (!rangesContain(next.verifiedRanges, previous.verifiedRanges)) {
    throw new FileCheckpointError('generation', 'verified checkpoint ranges regressed')
  }
  const sameCheckpointGeneration = next.checkpointGeneration === previous.checkpointGeneration
  if (sameCheckpointGeneration && !sameRanges(previous.verifiedRanges, next.verifiedRanges)) {
    throw new FileCheckpointError('generation', 'checkpoint ranges changed without a generation')
  }
  if (sameCheckpointGeneration && previous.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE) {
    if (next.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
        next.stateGeneration !== previous.stateGeneration || next.phase !== previous.phase) {
      throw new FileCheckpointError('generation', 'candidate promotion is ambiguous')
    }
    return
  }
  if (sameCheckpointGeneration &&
      (next.stateGeneration <= previous.stateGeneration ||
       !validCheckpointLifecycleTransition(previous, next))) {
    throw new FileCheckpointError('generation', 'checkpoint lifecycle transition is ambiguous')
  }
}

export function fileCheckpointIsComplete(record: FileCheckpointV2): boolean {
  if (record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED &&
      record.commitState !== FILE_CHECKPOINT_COMMIT_PUBLISHED) return false
  if (record.exactSize === 0n) return record.verifiedRanges.length === 0
  return record.verifiedRanges.length === 1 &&
    record.verifiedRanges[0]?.start === 0n &&
    record.verifiedRanges[0]?.end === record.exactSize
}

export function identityBytes(
  value: CheckpointIdentity,
  expectedLength: number,
  label: string,
): Uint8Array<ArrayBuffer> {
  let bytes: Uint8Array | undefined
  if (value instanceof Uint8Array) bytes = Uint8Array.from(value)
  else if (typeof value === 'string') bytes = decodeBase64Url(value)
  if (bytes === undefined || bytes.byteLength !== expectedLength ||
      bytes.every((byte) => byte === 0) ||
      (typeof value === 'string' && encodeBase64Url(bytes) !== value)) {
    throw new FileCheckpointError(
      'binding',
      `${label} must be a canonical non-zero ${expectedLength}-byte identity`,
    )
  }
  return Uint8Array.from(bytes) as Uint8Array<ArrayBuffer>
}

function uint64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > FILE_CHECKPOINT_MAX_FILE_SIZE) {
    throw new FileCheckpointError('binding', `checkpoint ${label} is not a u64`)
  }
  return value
}

function nonZeroUint64(value: bigint, label: string): bigint {
  const checked = uint64(value, label)
  if (checked === 0n) {
    throw new FileCheckpointError('generation', `checkpoint ${label} must not be zero`)
  }
  return checked
}

function snapshotCheckpointPath(
  path: readonly string[],
  materializerKind: FileCheckpointMaterializerKind,
): readonly string[] {
  try {
    // A named single-file FSA reservation is itself the materialization root.
    // Its coordinate is therefore empty; legacy kind 2 remains fenced out.
    if (materializerKind === FILE_CHECKPOINT_MATERIALIZER_FSA_TREE &&
        Array.isArray(path) && path.length === 0) return Object.freeze([])
    return snapshotPortableCatalogPath(path)
  } catch (cause) {
    throw new FileCheckpointError(
      'binding',
      cause instanceof Error ? `checkpoint path is invalid: ${cause.message}` : 'checkpoint path is invalid',
    )
  }
}

function validateRanges(
  ranges: readonly FileCheckpointRange[],
  exactSize: bigint,
): readonly FileCheckpointRange[] {
  if (!Array.isArray(ranges) || ranges.length > FILE_CHECKPOINT_MAX_RANGES) {
    throw new FileCheckpointError('invalid', 'too many checkpoint ranges')
  }
  const copied = ranges.map((range) => Object.freeze({ start: range.start, end: range.end }))
  for (let index = 0; index < copied.length; index += 1) {
    const current = copied[index]!
    const previous = copied[index - 1]
    if (typeof current.start !== 'bigint' || typeof current.end !== 'bigint' ||
        current.start < 0n || current.start >= current.end || current.end > exactSize) {
      throw new FileCheckpointError('invalid', 'checkpoint range is outside exact size')
    }
    if (previous !== undefined && current.start <= previous.end) {
      throw new FileCheckpointError(
        'invalid',
        'checkpoint ranges must be sorted, non-overlapping, and non-adjacent',
      )
    }
  }
  return Object.freeze(copied)
}

function rangesContain(
  candidates: readonly FileCheckpointRange[],
  required: readonly FileCheckpointRange[],
): boolean {
  return required.every((range) => candidates.some((candidate) =>
    candidate.start <= range.start && candidate.end >= range.end))
}

function sameRanges(
  left: readonly FileCheckpointRange[],
  right: readonly FileCheckpointRange[],
): boolean {
  return left.length === right.length && left.every((range, index) =>
    range.start === right[index]?.start && range.end === right[index]?.end)
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}
