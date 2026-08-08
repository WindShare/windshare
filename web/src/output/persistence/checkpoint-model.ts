import { canonicalizePortableCatalogPath } from '../../catalog/path-policy'
import { decodeBase64Url } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_PUBLISHED,
  FILE_CHECKPOINT_MAX_QUARANTINE_ORIGIN,
  FILE_CHECKPOINT_MAX_QUARANTINE_REASON,
  FILE_CHECKPOINT_MAX_RETIREMENT_REASON,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PUBLISHED,
  FILE_CHECKPOINT_PHASE_PUBLISHING,
  FileCheckpointError,
  normalizeCheckpointCommitState,
  normalizeCheckpointLifecycleClaim,
  normalizeCheckpointPhase,
  validCheckpointLifecycleTransition,
  validateCheckpointLifecycleClaims,
  type FileCheckpointCommitState,
  type FileCheckpointPhase,
  type FileCheckpointQuarantineOrigin,
  type FileCheckpointQuarantineReason,
  type FileCheckpointRetirementReason,
} from './checkpoint-lifecycle'

export const FILE_CHECKPOINT_V1_SCHEMA_VERSION = 1 as const
export const FILE_CHECKPOINT_OWNERSHIP_MARKER = 'windshare/file-checkpoint/v1' as const
export const FILE_CHECKPOINT_NAMESPACE = '.windshare-output/checkpoints-v1' as const

export const FILE_CHECKPOINT_MAX_RANGES = 16_384
export const FILE_CHECKPOINT_MAX_PATH_BYTES = 32 * 1024
export const FILE_CHECKPOINT_MAX_BACKEND_BYTES = 128
export const FILE_CHECKPOINT_MAX_FILE_SIZE = (1n << 53n) - 1n
export const FILE_CHECKPOINT_ID_BYTES = 32
export const FILE_ID_BYTES = 16
export const FILE_REVISION_BYTES = 16

const MAX_UINT64 = 0xffff_ffff_ffff_ffffn

export interface FileCheckpointRange {
  readonly start: bigint
  readonly end: bigint
}

/** Go-facing spellings keep generated-vector assertions independent of adapters. */
export type CheckpointRange = FileCheckpointRange

export type CheckpointIdentity = string | Uint8Array

export interface FileCheckpointSpec {
  readonly ownershipMarker?: string
  readonly namespace?: string
  /** Base64url of the 32-byte TransferIntentDigest (or the raw 32 bytes). */
  readonly transferIntentDigest: CheckpointIdentity
  /** Base64url of the 16-byte catalog FileID (or the raw 16 bytes). */
  readonly fileId: CheckpointIdentity
  /** Base64url of the 16-byte content FileRevision (or the raw 16 bytes). */
  readonly fileRevision: CheckpointIdentity
  /** A canonical slash-separated path, or its output path segments. */
  readonly canonicalPath: string | readonly string[]
  readonly exactSize: bigint
  readonly backend: string
  /** Base64url of a 32-byte receiver root identity (or raw bytes). */
  readonly rootIdentity: CheckpointIdentity
  /** Base64url of a 32-byte owned output object identity (or raw bytes). */
  readonly ownedOutputObject: CheckpointIdentity
  readonly stateGeneration: bigint
  readonly checkpointGeneration: bigint
  readonly verifiedRanges: readonly FileCheckpointRange[]
  readonly phase?: FileCheckpointPhase | number | string
  readonly commitState?: FileCheckpointCommitState | number | string
  readonly quarantineReason?: FileCheckpointQuarantineReason | number
  readonly quarantineOrigin?: FileCheckpointQuarantineOrigin | number
  readonly retirementReason?: FileCheckpointRetirementReason | number
}

export interface FileCheckpointV1 {
  readonly schemaVersion: typeof FILE_CHECKPOINT_V1_SCHEMA_VERSION
  readonly ownershipMarker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespace: typeof FILE_CHECKPOINT_NAMESPACE
  readonly recordId: string
  readonly transferIntentDigest: string
  readonly fileId: string
  readonly fileRevision: string
  readonly canonicalPath: string
  readonly exactSize: bigint
  readonly backend: string
  readonly rootIdentity: string
  readonly ownedOutputObject: string
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
  readonly transferIntentDigest: Uint8Array<ArrayBuffer>
  readonly fileId: Uint8Array<ArrayBuffer>
  readonly fileRevision: Uint8Array<ArrayBuffer>
  readonly canonicalPath: string
  readonly exactSize: bigint
  readonly backend: string
  readonly rootIdentity: Uint8Array<ArrayBuffer>
  readonly ownedOutputObject: Uint8Array<ArrayBuffer>
  readonly stateGeneration: bigint
  readonly checkpointGeneration: bigint
  readonly verifiedRanges: FileCheckpointRange[]
  readonly phase: FileCheckpointPhase
  readonly commitState: FileCheckpointCommitState
  readonly quarantineReason: FileCheckpointQuarantineReason | 0
  readonly quarantineOrigin: FileCheckpointQuarantineOrigin | 0
  readonly retirementReason: FileCheckpointRetirementReason | 0
}

export function normalizeFileCheckpointSpec(spec: FileCheckpointSpec): NormalizedFileCheckpointSpec {
  const ownershipMarker = spec.ownershipMarker ?? FILE_CHECKPOINT_OWNERSHIP_MARKER
  const namespace = spec.namespace ?? FILE_CHECKPOINT_NAMESPACE
  if (ownershipMarker !== FILE_CHECKPOINT_OWNERSHIP_MARKER || namespace !== FILE_CHECKPOINT_NAMESPACE) {
    throw new FileCheckpointError('ownership', 'FileCheckpointV1 ownership marker or namespace is invalid')
  }
  const transferIntentDigest = identityBytes(
    spec.transferIntentDigest,
    FILE_CHECKPOINT_ID_BYTES,
    'transfer intent digest',
  )
  const fileId = identityBytes(spec.fileId, FILE_ID_BYTES, 'file ID')
  const fileRevision = identityBytes(spec.fileRevision, FILE_REVISION_BYTES, 'file revision')
  const rootIdentity = identityBytes(spec.rootIdentity, FILE_CHECKPOINT_ID_BYTES, 'root identity')
  const ownedOutputObject = identityBytes(
    spec.ownedOutputObject,
    FILE_CHECKPOINT_ID_BYTES,
    'owned output object',
  )
  const canonicalPath = canonicalCheckpointPath(spec.canonicalPath)
  const backend = canonicalFileCheckpointBackend(spec.backend)
  if (typeof spec.exactSize !== 'bigint' || spec.exactSize < 0n ||
      spec.exactSize > FILE_CHECKPOINT_MAX_FILE_SIZE) {
    throw new FileCheckpointError('binding', 'checkpoint exact size is invalid')
  }
  if (typeof spec.stateGeneration !== 'bigint' || spec.stateGeneration <= 0n ||
      spec.stateGeneration > MAX_UINT64 ||
      typeof spec.checkpointGeneration !== 'bigint' || spec.checkpointGeneration < 0n ||
      spec.checkpointGeneration > MAX_UINT64) {
    throw new FileCheckpointError('generation', 'checkpoint generation is invalid')
  }
  const verifiedRanges = validateRanges(spec.verifiedRanges, spec.exactSize)
  const phase = normalizeCheckpointPhase(spec.phase ?? FILE_CHECKPOINT_PHASE_ACTIVE)
  const commitState = normalizeCheckpointCommitState(spec.commitState ?? FILE_CHECKPOINT_COMMIT_CANDIDATE)
  const quarantineReason = normalizeCheckpointLifecycleClaim<FileCheckpointQuarantineReason>(
    spec.quarantineReason ?? 0,
    FILE_CHECKPOINT_MAX_QUARANTINE_REASON,
    'quarantine reason',
  )
  const quarantineOrigin = normalizeCheckpointLifecycleClaim<FileCheckpointQuarantineOrigin>(
    spec.quarantineOrigin ?? 0,
    FILE_CHECKPOINT_MAX_QUARANTINE_ORIGIN,
    'quarantine origin',
  )
  const retirementReason = normalizeCheckpointLifecycleClaim<FileCheckpointRetirementReason>(
    spec.retirementReason ?? 0,
    FILE_CHECKPOINT_MAX_RETIREMENT_REASON,
    'retirement reason',
  )
  validateCheckpointLifecycleClaims(phase, quarantineReason, quarantineOrigin, retirementReason)
  return {
    ownershipMarker,
    namespace,
    transferIntentDigest,
    fileId,
    fileRevision,
    canonicalPath,
    exactSize: spec.exactSize,
    backend,
    rootIdentity,
    ownedOutputObject,
    stateGeneration: spec.stateGeneration,
    checkpointGeneration: spec.checkpointGeneration,
    verifiedRanges,
    phase,
    commitState,
    quarantineReason,
    quarantineOrigin,
    retirementReason,
  }
}

export function checkpointIdentityEqual(left: FileCheckpointV1, right: FileCheckpointV1): boolean {
  return left.recordId === right.recordId &&
    left.transferIntentDigest === right.transferIntentDigest &&
    left.fileId === right.fileId && left.fileRevision === right.fileRevision &&
    left.canonicalPath === right.canonicalPath && left.exactSize === right.exactSize &&
    left.backend === right.backend && left.rootIdentity === right.rootIdentity &&
    left.ownedOutputObject === right.ownedOutputObject
}

export function validateCheckpointTransitionSemantics(
  previous: FileCheckpointV1,
  next: FileCheckpointV1,
): void {
  if (!checkpointIdentityEqual(previous, next)) {
    throw new FileCheckpointError('binding', 'checkpoint identity changed')
  }
  if (next.stateGeneration < previous.stateGeneration ||
      next.checkpointGeneration < previous.checkpointGeneration) {
    throw new FileCheckpointError('generation', 'checkpoint generation regressed')
  }
  if (next.checkpointGeneration === previous.checkpointGeneration) {
    const invalidCandidatePromotion = previous.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE &&
      (next.commitState <= previous.commitState || next.stateGeneration !== previous.stateGeneration ||
       !sameCheckpointRanges(previous.verifiedRanges, next.verifiedRanges) ||
       (previous.phase !== next.phase &&
        !(previous.phase === FILE_CHECKPOINT_PHASE_PUBLISHING && next.phase === FILE_CHECKPOINT_PHASE_PUBLISHED)))
    const invalidLifecycleAdvance = previous.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE &&
      (next.stateGeneration <= previous.stateGeneration ||
       !sameCheckpointRanges(previous.verifiedRanges, next.verifiedRanges) ||
       !validCheckpointLifecycleTransition(previous, next))
    if (invalidCandidatePromotion || invalidLifecycleAdvance) {
      throw new FileCheckpointError('generation', 'same-generation checkpoint transition is ambiguous')
    }
  }
  if (previous.commitState === FILE_CHECKPOINT_COMMIT_PUBLISHED ||
      previous.phase === FILE_CHECKPOINT_PHASE_PUBLISHED) {
    throw new FileCheckpointError('generation', 'published checkpoint is immutable')
  }
  for (const required of previous.verifiedRanges) {
    if (!next.verifiedRanges.some((candidate) =>
      candidate.start <= required.start && candidate.end >= required.end)) {
      throw new FileCheckpointError('generation', 'verified ranges regressed')
    }
  }
}

export function canonicalizeFileCheckpointRanges(
  ranges: readonly FileCheckpointRange[],
): FileCheckpointRange[] {
  const sorted = [...ranges]
    .map((range) => ({ start: range.start, end: range.end }))
    .sort(compareCheckpointRanges)
  const merged: FileCheckpointRange[] = []
  for (const current of sorted) {
    if (current.start < 0n || current.end <= current.start) {
      throw new FileCheckpointError('invalid', 'checkpoint range is empty or negative')
    }
    const previous = merged.at(-1)
    if (previous === undefined || current.start > previous.end) merged.push(current)
    else if (current.end > previous.end) {
      merged[merged.length - 1] = { start: previous.start, end: current.end }
    }
  }
  return merged
}

export function identityBytes(
  value: CheckpointIdentity,
  expectedLength: number,
  label: string,
): Uint8Array<ArrayBuffer> {
  let bytes: Uint8Array | undefined
  if (value instanceof Uint8Array) bytes = Uint8Array.from(value)
  else if (typeof value === 'string') bytes = decodeBase64Url(value)
  if (bytes === undefined || bytes.byteLength !== expectedLength) {
    throw new FileCheckpointError(
      'binding',
      `${label} must be ${expectedLength} bytes in canonical base64url form`,
    )
  }
  if (bytes.every((byte) => byte === 0)) {
    throw new FileCheckpointError('binding', `${label} must not be zero`)
  }
  return Uint8Array.from(bytes) as Uint8Array<ArrayBuffer>
}

export function canonicalFileCheckpointBackend(value: string): string {
  if (typeof value !== 'string' || value.length === 0 || !isWellFormedUnicode(value) ||
      new TextEncoder().encode(value).byteLength > FILE_CHECKPOINT_MAX_BACKEND_BYTES) {
    throw new FileCheckpointError('binding', 'checkpoint backend is invalid')
  }
  const characters = [...value]
  const first = characters[0]?.codePointAt(0) ?? 0
  const last = characters.at(-1)?.codePointAt(0) ?? 0
  if (isGoSpace(first) || isGoSpace(last)) {
    throw new FileCheckpointError('binding', 'checkpoint backend is invalid')
  }
  return value
}

function compareCheckpointRanges(left: FileCheckpointRange, right: FileCheckpointRange): number {
  if (left.start !== right.start) return left.start < right.start ? -1 : 1
  if (left.end === right.end) return 0
  return left.end < right.end ? -1 : 1
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (next < 0xdc00 || next > 0xdfff) return false
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) return false
  }
  return true
}

function isGoSpace(value: number): boolean {
  return (value >= 0x09 && value <= 0x0d) || value === 0x20 || value === 0x85 || value === 0xa0 ||
    value === 0x1680 || (value >= 0x2000 && value <= 0x200a) || value === 0x2028 || value === 0x2029 ||
    value === 0x202f || value === 0x205f || value === 0x3000
}

function canonicalCheckpointPath(path: string | readonly string[]): string {
  const value = Array.isArray(path) ? path.join('/') : path
  if (typeof value !== 'string' || value.length === 0 || value.includes('\0')) {
    throw new FileCheckpointError('binding', 'checkpoint path is not relative and canonical')
  }
  try {
    const normalized = canonicalizePortableCatalogPath(value)
    if (normalized !== value) throw new Error('path is not already NFC canonical')
    if (new TextEncoder().encode(normalized).byteLength > FILE_CHECKPOINT_MAX_PATH_BYTES) {
      throw new Error('too long')
    }
    return normalized
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    throw new FileCheckpointError('binding', `checkpoint path is not canonical: ${reason}`)
  }
}

function validateRanges(
  ranges: readonly FileCheckpointRange[],
  exactSize: bigint,
): FileCheckpointRange[] {
  if (ranges.length > FILE_CHECKPOINT_MAX_RANGES) {
    throw new FileCheckpointError('invalid', 'too many checkpoint ranges')
  }
  const copied = ranges.map((range) => ({ start: range.start, end: range.end }))
  for (let index = 0; index < copied.length; index += 1) {
    const current = copied[index]!
    if (current.start < 0n || current.start >= current.end || current.end > exactSize) {
      throw new FileCheckpointError('invalid', 'checkpoint range is outside exact file size')
    }
    const previous = copied[index - 1]
    if (previous !== undefined && current.start <= previous.end) {
      throw new FileCheckpointError(
        'invalid',
        'checkpoint ranges must be sorted, non-overlapping, and non-adjacent',
      )
    }
  }
  return copied
}

function sameCheckpointRanges(
  left: readonly FileCheckpointRange[],
  right: readonly FileCheckpointRange[],
): boolean {
  return left.length === right.length && left.every((range, index) =>
    range.start === right[index]!.start && range.end === right[index]!.end)
}
