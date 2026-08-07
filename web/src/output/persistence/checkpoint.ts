import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import { canonicalizePortableCatalogPath } from '../../catalog/path-policy'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_PUBLISHED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
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

export * from './checkpoint-lifecycle'

/**
 * FileCheckpointV1 is the cross-backend recovery value.  The binary envelope in
 * this module deliberately mirrors core/osfs/internal/resumestate/checkpoint_v1.go:
 * a backend may choose a different physical store, but it cannot choose a second
 * interpretation of a verified range or a durable identity.
 */
export const FILE_CHECKPOINT_V1_SCHEMA_VERSION = 1 as const
export const FILE_CHECKPOINT_OWNERSHIP_MARKER = 'windshare/file-checkpoint/v1' as const
export const FILE_CHECKPOINT_NAMESPACE = '.windshare-output/checkpoints-v1' as const
export const FILE_CHECKPOINT_RECORD_DOMAIN = 'windshare/file-checkpoint-record/v1' as const
export const FILE_CHECKPOINT_CHECKSUM_DOMAIN = 'windshare/file-checkpoint-checksum/v1' as const
export const FILE_CHECKPOINT_OWNERSHIP_DOMAIN = 'windshare/file-checkpoint-ownership/v1' as const
export const FILE_CHECKPOINT_MAGIC = new TextEncoder().encode('WSFCPV1\0')
const FILE_CHECKPOINT_TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })

export const FILE_CHECKPOINT_MAX_RANGES = 16_384
export const FILE_CHECKPOINT_MAX_PATH_BYTES = 32 * 1024
export const FILE_CHECKPOINT_MAX_BACKEND_BYTES = 128
export const FILE_CHECKPOINT_MAX_FILE_SIZE = (1n << 53n) - 1n
export const FILE_CHECKPOINT_ID_BYTES = 32
export const FILE_ID_BYTES = 16
export const FILE_REVISION_BYTES = 16

export interface FileCheckpointRange {
  readonly start: bigint
  readonly end: bigint
}

/** Go-facing spellings are useful when a test consumes a generated vector. */
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
  /** Compatibility is restricted to the old in-memory test adapters. */
  readonly allowReadableIdentities?: boolean
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

export interface FileCheckpointOwnership {
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespace: typeof FILE_CHECKPOINT_NAMESPACE
  readonly backend: string
  readonly rootIdentity: string
}

/** Construct an immutable record; this function never repairs persisted input. */
export function newFileCheckpointV1(spec: FileCheckpointSpec): FileCheckpointV1 {
  const marker = spec.ownershipMarker ?? FILE_CHECKPOINT_OWNERSHIP_MARKER
  const namespace = spec.namespace ?? FILE_CHECKPOINT_NAMESPACE
  if (marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER || namespace !== FILE_CHECKPOINT_NAMESPACE) {
    throw new FileCheckpointError('ownership', 'FileCheckpointV1 ownership marker or namespace is invalid')
  }
  const allowReadable = spec.allowReadableIdentities === true
  const intent = fixedIdentity(spec.transferIntentDigest, FILE_CHECKPOINT_ID_BYTES, 'transfer intent digest', allowReadable)
  const fileId = fixedIdentity(spec.fileId, FILE_ID_BYTES, 'file ID', allowReadable)
  const revision = fixedIdentity(spec.fileRevision, FILE_REVISION_BYTES, 'file revision', allowReadable)
  const root = fixedIdentity(spec.rootIdentity, FILE_CHECKPOINT_ID_BYTES, 'root identity', allowReadable)
  const object = fixedIdentity(spec.ownedOutputObject, FILE_CHECKPOINT_ID_BYTES, 'owned output object', allowReadable)
  const canonicalPath = canonicalCheckpointPath(spec.canonicalPath)
  const backend = canonicalCheckpointBackend(spec.backend)
  if (typeof spec.exactSize !== 'bigint' || spec.exactSize < 0n || spec.exactSize > FILE_CHECKPOINT_MAX_FILE_SIZE) {
    throw new FileCheckpointError('binding', 'checkpoint exact size is invalid')
  }
  if (typeof spec.stateGeneration !== 'bigint' || spec.stateGeneration <= 0n ||
      spec.stateGeneration > 0xffff_ffff_ffff_ffffn ||
      typeof spec.checkpointGeneration !== 'bigint' || spec.checkpointGeneration < 0n ||
      spec.checkpointGeneration > 0xffff_ffff_ffff_ffffn) {
    throw new FileCheckpointError('generation', 'checkpoint generation is invalid')
  }
  const ranges = validateRanges(spec.verifiedRanges, spec.exactSize)
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
  const identityPayload = concatBytes([
    framedText(FILE_CHECKPOINT_RECORD_DOMAIN),
    framedBytes(Uint8Array.of(FILE_CHECKPOINT_V1_SCHEMA_VERSION)),
    framedText(marker),
    framedText(namespace),
    framedBytes(intent),
    framedBytes(fileId),
    framedBytes(revision),
    framedText(canonicalPath),
    uint64Bytes(spec.exactSize),
    framedText(backend),
    framedBytes(root),
    framedBytes(object),
  ])
  const recordId = sha256Sync(identityPayload)
  const withoutChecksum: Omit<FileCheckpointV1, 'checksum'> = {
    schemaVersion: FILE_CHECKPOINT_V1_SCHEMA_VERSION,
    ownershipMarker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespace: FILE_CHECKPOINT_NAMESPACE,
    recordId: encodeBase64Url(recordId),
    transferIntentDigest: encodeBase64Url(intent),
    fileId: encodeBase64Url(fileId),
    fileRevision: encodeBase64Url(revision),
    canonicalPath,
    exactSize: spec.exactSize,
    backend,
    rootIdentity: encodeBase64Url(root),
    ownedOutputObject: encodeBase64Url(object),
    stateGeneration: spec.stateGeneration,
    checkpointGeneration: spec.checkpointGeneration,
    verifiedRanges: Object.freeze(ranges),
    phase,
    commitState,
    quarantineReason,
    quarantineOrigin,
    retirementReason,
  }
  return Object.freeze({
    ...withoutChecksum,
    checksum: encodeBase64Url(checksumFor(withoutChecksum)),
  })
}

export const createFileCheckpointV1 = newFileCheckpointV1
export const newFileCheckpointRecord = newFileCheckpointV1

/** Deterministic opaque identity used only to derive a namespace fallback or metadata key. */
export function deriveCheckpointIdentity(value: string): string {
  return encodeBase64Url(sha256Sync(new TextEncoder().encode(value)))
}

export function canonicalFileCheckpointBytes(record: FileCheckpointV1): Uint8Array<ArrayBuffer> {
  validateFileCheckpoint(record)
  return canonicalPayload(record)
}

export function encodeFileCheckpointV1(record: FileCheckpointV1): Uint8Array<ArrayBuffer> {
  validateFileCheckpoint(record)
  const payload = canonicalPayload(record)
  const output = new Uint8Array(FILE_CHECKPOINT_MAGIC.byteLength + 4 + payload.byteLength + 32)
  output.set(FILE_CHECKPOINT_MAGIC, 0)
  new DataView(output.buffer).setUint32(FILE_CHECKPOINT_MAGIC.byteLength, payload.byteLength, false)
  output.set(payload, FILE_CHECKPOINT_MAGIC.byteLength + 4)
  output.set(checksumFor(record), FILE_CHECKPOINT_MAGIC.byteLength + 4 + payload.byteLength)
  return output
}

export const encodeFileCheckpointRecord = encodeFileCheckpointV1

export function decodeFileCheckpointV1(encoded: Uint8Array): FileCheckpointV1 {
  const minimum = FILE_CHECKPOINT_MAGIC.byteLength + 4 + 1 + 32
  if (encoded.byteLength < minimum || !equalBytes(encoded.subarray(0, FILE_CHECKPOINT_MAGIC.byteLength), FILE_CHECKPOINT_MAGIC)) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV1 envelope is invalid')
  }
  const payloadLength = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength).getUint32(FILE_CHECKPOINT_MAGIC.byteLength, false)
  const payloadStart = FILE_CHECKPOINT_MAGIC.byteLength + 4
  const payloadEnd = payloadStart + payloadLength
  if (payloadEnd + 32 !== encoded.byteLength) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV1 payload length is invalid')
  }
  const payload = encoded.subarray(payloadStart, payloadEnd)
  const suppliedChecksum = encoded.subarray(payloadEnd)
  if (!equalBytes(suppliedChecksum, sha256Sync(concatBytes([new TextEncoder().encode(`${FILE_CHECKPOINT_CHECKSUM_DOMAIN}\0`), payload])))) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV1 checksum is invalid')
  }
  const cursor = new CheckpointCursor(payload)
  const domain = cursor.text(FILE_CHECKPOINT_RECORD_DOMAIN.length)
  if (domain !== FILE_CHECKPOINT_RECORD_DOMAIN) throw new FileCheckpointError('non-canonical', 'FileCheckpointV1 domain is invalid')
  const version = cursor.byte()
  if (version !== FILE_CHECKPOINT_V1_SCHEMA_VERSION) throw new FileCheckpointError('invalid', 'FileCheckpointV1 schema version is invalid')
  const marker = cursor.text(128)
  const namespace = cursor.text(256)
  const recordId = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'record ID')
  const intent = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'transfer intent digest')
  const fileId = cursor.fixed(FILE_ID_BYTES, 'file ID')
  const revision = cursor.fixed(FILE_REVISION_BYTES, 'file revision')
  const path = cursor.text(FILE_CHECKPOINT_MAX_PATH_BYTES)
  const exactSize = cursor.u64()
  const backend = cursor.text(FILE_CHECKPOINT_MAX_BACKEND_BYTES)
  const root = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'root identity')
  const object = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'owned output object')
  const stateGeneration = cursor.u64()
  const checkpointGeneration = cursor.u64()
  const count = cursor.u32()
  if (count > FILE_CHECKPOINT_MAX_RANGES) throw new FileCheckpointError('invalid', 'too many checkpoint ranges')
  const ranges: FileCheckpointRange[] = []
  for (let index = 0; index < count; index += 1) ranges.push({ start: cursor.u64(), end: cursor.u64() })
  const phase = cursor.byte()
  const commitState = cursor.byte()
  const quarantineReason = cursor.byte()
  const quarantineOrigin = cursor.byte()
  const retirementReason = cursor.byte()
  if (!cursor.done()) throw new FileCheckpointError('non-canonical', 'FileCheckpointV1 payload has trailing bytes')
  const record = newFileCheckpointV1({
    ownershipMarker: marker,
    namespace,
    transferIntentDigest: intent,
    fileId,
    fileRevision: revision,
    canonicalPath: path,
    exactSize,
    backend,
    rootIdentity: root,
    ownedOutputObject: object,
    stateGeneration,
    checkpointGeneration,
    verifiedRanges: ranges,
    phase,
    commitState,
    quarantineReason,
    quarantineOrigin,
    retirementReason,
  })
  if (record.recordId !== encodeBase64Url(recordId)) {
    throw new FileCheckpointError('binding', 'FileCheckpointV1 record ID does not match immutable identity')
  }
  if (record.checksum !== encodeBase64Url(suppliedChecksum)) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV1 checksum does not match payload')
  }
  if (!equalBytes(canonicalPayload(record), payload)) throw new FileCheckpointError('non-canonical', 'FileCheckpointV1 encoding is not canonical')
  return record
}

export const decodeFileCheckpointRecord = decodeFileCheckpointV1

export function validateFileCheckpoint(record: FileCheckpointV1): void {
  const rebuilt = newFileCheckpointV1({
    ownershipMarker: record.ownershipMarker,
    namespace: record.namespace,
    transferIntentDigest: record.transferIntentDigest,
    fileId: record.fileId,
    fileRevision: record.fileRevision,
    canonicalPath: record.canonicalPath,
    exactSize: record.exactSize,
    backend: record.backend,
    rootIdentity: record.rootIdentity,
    ownedOutputObject: record.ownedOutputObject,
    stateGeneration: record.stateGeneration,
    checkpointGeneration: record.checkpointGeneration,
    verifiedRanges: record.verifiedRanges,
    phase: record.phase,
    commitState: record.commitState,
    quarantineReason: record.quarantineReason,
    quarantineOrigin: record.quarantineOrigin,
    retirementReason: record.retirementReason,
  })
  if (rebuilt.recordId !== record.recordId) throw new FileCheckpointError('binding', 'FileCheckpointV1 record ID is invalid')
  if (rebuilt.checksum !== record.checksum) throw new FileCheckpointError('checksum', 'FileCheckpointV1 checksum is invalid')
}

export function checkpointIdentityEqual(left: FileCheckpointV1, right: FileCheckpointV1): boolean {
  return left.recordId === right.recordId &&
    left.transferIntentDigest === right.transferIntentDigest &&
    left.fileId === right.fileId && left.fileRevision === right.fileRevision &&
    left.canonicalPath === right.canonicalPath && left.exactSize === right.exactSize &&
    left.backend === right.backend && left.rootIdentity === right.rootIdentity &&
    left.ownedOutputObject === right.ownedOutputObject
}

export function validateFileCheckpointTransition(previous: FileCheckpointV1, next: FileCheckpointV1): void {
  validateFileCheckpoint(previous)
  validateFileCheckpoint(next)
  if (!checkpointIdentityEqual(previous, next)) throw new FileCheckpointError('binding', 'checkpoint identity changed')
  if (next.stateGeneration < previous.stateGeneration || next.checkpointGeneration < previous.checkpointGeneration) {
    throw new FileCheckpointError('generation', 'checkpoint generation regressed')
  }
  if (next.checkpointGeneration === previous.checkpointGeneration) {
    const invalidCandidatePromotion = previous.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE &&
      (next.commitState <= previous.commitState || next.stateGeneration !== previous.stateGeneration ||
       !sameRanges(previous.verifiedRanges, next.verifiedRanges) ||
       (previous.phase !== next.phase &&
        !(previous.phase === FILE_CHECKPOINT_PHASE_PUBLISHING && next.phase === FILE_CHECKPOINT_PHASE_PUBLISHED)))
    const invalidLifecycleAdvance = previous.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE &&
      (next.stateGeneration <= previous.stateGeneration ||
       !sameRanges(previous.verifiedRanges, next.verifiedRanges) ||
       !validCheckpointLifecycleTransition(previous, next))
    if (invalidCandidatePromotion || invalidLifecycleAdvance) {
      throw new FileCheckpointError('generation', 'same-generation checkpoint transition is ambiguous')
    }
  }
  if (previous.commitState === FILE_CHECKPOINT_COMMIT_PUBLISHED || previous.phase === FILE_CHECKPOINT_PHASE_PUBLISHED) {
    throw new FileCheckpointError('generation', 'published checkpoint is immutable')
  }
  for (const required of previous.verifiedRanges) {
    if (!next.verifiedRanges.some((candidate) => candidate.start <= required.start && candidate.end >= required.end)) {
      throw new FileCheckpointError('generation', 'verified ranges regressed')
    }
  }
}

export function selectVerifiedCheckpoint(...records: readonly FileCheckpointV1[]): FileCheckpointV1 {
  let selected: FileCheckpointV1 | undefined
  for (const record of records) {
    validateFileCheckpoint(record)
    if (record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED && record.commitState !== FILE_CHECKPOINT_COMMIT_PUBLISHED) continue
    if (selected === undefined) {
      selected = record
      continue
    }
    if (!checkpointIdentityEqual(selected, record)) {
      throw new FileCheckpointError('binding', 'committed checkpoints do not share an immutable identity')
    }
    if (record.checkpointGeneration > selected.checkpointGeneration) {
      selected = record
    } else if (record.checkpointGeneration === selected.checkpointGeneration &&
        !equalBytes(canonicalPayload(record), canonicalPayload(selected))) {
      throw new FileCheckpointError('crash-boundary', 'committed checkpoints disagree at one generation')
    }
  }
  if (selected === undefined) throw new FileCheckpointError('recovery', 'no verified committed checkpoint exists')
  return selected
}

export function canonicalizeFileCheckpointRanges(ranges: readonly FileCheckpointRange[]): FileCheckpointRange[] {
  const sorted = [...ranges]
    .map((range) => ({ start: range.start, end: range.end }))
    .sort(compareCheckpointRanges)
  const merged: FileCheckpointRange[] = []
  for (const current of sorted) {
    if (current.start < 0n || current.end <= current.start) throw new FileCheckpointError('invalid', 'checkpoint range is empty or negative')
    const previous = merged.at(-1)
    if (previous === undefined || current.start > previous.end) merged.push(current)
    else if (current.end > previous.end) merged[merged.length - 1] = { start: previous.start, end: current.end }
  }
  return merged
}

function compareCheckpointRanges(left: FileCheckpointRange, right: FileCheckpointRange): number {
  if (left.start !== right.start) return left.start < right.start ? -1 : 1
  if (left.end === right.end) return 0
  return left.end < right.end ? -1 : 1
}

export function encodeFileCheckpointOwnership(ownership: FileCheckpointOwnership): Uint8Array<ArrayBuffer> {
  if (ownership.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER || ownership.namespace !== FILE_CHECKPOINT_NAMESPACE) throw new FileCheckpointError('ownership', 'checkpoint ownership marker is invalid')
  const root = fixedIdentity(ownership.rootIdentity, FILE_CHECKPOINT_ID_BYTES, 'root identity')
  const backend = canonicalCheckpointBackend(ownership.backend)
  const payload = concatBytes([framedText(FILE_CHECKPOINT_OWNERSHIP_DOMAIN), framedText(ownership.marker), framedText(ownership.namespace), framedText(backend), root])
  return concatBytes([payload, sha256Sync(concatBytes([new TextEncoder().encode(`${FILE_CHECKPOINT_CHECKSUM_DOMAIN}\0`), payload]))])
}

export function decodeFileCheckpointOwnership(encoded: Uint8Array): FileCheckpointOwnership {
  if (encoded.byteLength < 33) throw new FileCheckpointError('ownership', 'checkpoint ownership marker is truncated')
  const payload = encoded.subarray(0, encoded.byteLength - 32)
  const supplied = encoded.subarray(encoded.byteLength - 32)
  const expected = sha256Sync(concatBytes([new TextEncoder().encode(`${FILE_CHECKPOINT_CHECKSUM_DOMAIN}\0`), payload]))
  if (!equalBytes(supplied, expected)) throw new FileCheckpointError('checksum', 'checkpoint ownership checksum is invalid')
  const cursor = new CheckpointCursor(payload)
  if (cursor.text(FILE_CHECKPOINT_OWNERSHIP_DOMAIN.length) !== FILE_CHECKPOINT_OWNERSHIP_DOMAIN) throw new FileCheckpointError('ownership', 'checkpoint ownership domain is invalid')
  const marker = cursor.text(128)
  const namespace = cursor.text(256)
  const backend = canonicalCheckpointBackend(cursor.text(FILE_CHECKPOINT_MAX_BACKEND_BYTES))
  const root = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'root identity')
  if (!cursor.done() || marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER || namespace !== FILE_CHECKPOINT_NAMESPACE) throw new FileCheckpointError('ownership', 'checkpoint ownership marker is invalid')
  const ownership = Object.freeze({ marker: FILE_CHECKPOINT_OWNERSHIP_MARKER, namespace: FILE_CHECKPOINT_NAMESPACE, backend, rootIdentity: encodeBase64Url(root) })
  if (!equalBytes(encodeFileCheckpointOwnership(ownership), encoded)) {
    throw new FileCheckpointError('non-canonical', 'checkpoint ownership encoding is not canonical')
  }
  return ownership
}

export function identityBytes(value: CheckpointIdentity, expectedLength: number, label: string, allowReadable = false): Uint8Array<ArrayBuffer> {
  return fixedIdentity(value, expectedLength, label, allowReadable)
}

function fixedIdentity(value: CheckpointIdentity, expectedLength: number, label: string, allowReadable = false): Uint8Array<ArrayBuffer> {
  let bytes: Uint8Array | undefined
  if (value instanceof Uint8Array) bytes = Uint8Array.from(value)
  else if (typeof value === 'string') bytes = decodeBase64Url(value)
  if (bytes === undefined || bytes.byteLength !== expectedLength) {
    if (!allowReadable || typeof value !== 'string' || value.length === 0) throw new FileCheckpointError('binding', `${label} must be ${expectedLength} bytes in canonical base64url form`)
    bytes = sha256Sync(new TextEncoder().encode(`windshare/checkpoint-test-identity/v1\0${value}`)).subarray(0, expectedLength)
  }
  if (bytes.every((byte) => byte === 0)) throw new FileCheckpointError('binding', `${label} must not be zero`)
  return Uint8Array.from(bytes) as Uint8Array<ArrayBuffer>
}

function canonicalCheckpointBackend(value: string): string {
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
  if (typeof value !== 'string' || value.length === 0 || value.includes('\0')) throw new FileCheckpointError('binding', 'checkpoint path is not relative and canonical')
  try {
    const normalized = canonicalizePortableCatalogPath(value)
    if (normalized !== value) throw new Error('path is not already NFC canonical')
    if (new TextEncoder().encode(normalized).byteLength > FILE_CHECKPOINT_MAX_PATH_BYTES) throw new Error('too long')
    return normalized
  } catch (error) {
    throw new FileCheckpointError('binding', `checkpoint path is not canonical: ${error instanceof Error ? error.message : String(error)}`)
  }
}

function validateRanges(ranges: readonly FileCheckpointRange[], exactSize: bigint): FileCheckpointRange[] {
  if (ranges.length > FILE_CHECKPOINT_MAX_RANGES) throw new FileCheckpointError('invalid', 'too many checkpoint ranges')
  const copied = ranges.map((range) => ({ start: range.start, end: range.end }))
  for (let index = 0; index < copied.length; index += 1) {
    const current = copied[index]!
    if (current.start < 0n || current.start >= current.end || current.end > exactSize) throw new FileCheckpointError('invalid', 'checkpoint range is outside exact file size')
    const previous = copied[index - 1]
    if (previous !== undefined && current.start <= previous.end) throw new FileCheckpointError('invalid', 'checkpoint ranges must be sorted, non-overlapping, and non-adjacent')
  }
  return copied
}

function canonicalPayload(record: FileCheckpointV1): Uint8Array<ArrayBuffer> {
  return concatBytes([
    framedText(FILE_CHECKPOINT_RECORD_DOMAIN),
    Uint8Array.of(FILE_CHECKPOINT_V1_SCHEMA_VERSION),
    framedText(record.ownershipMarker),
    framedText(record.namespace),
    fixedIdentity(record.recordId, FILE_CHECKPOINT_ID_BYTES, 'record ID'),
    fixedIdentity(record.transferIntentDigest, FILE_CHECKPOINT_ID_BYTES, 'transfer intent digest'),
    fixedIdentity(record.fileId, FILE_ID_BYTES, 'file ID'),
    fixedIdentity(record.fileRevision, FILE_REVISION_BYTES, 'file revision'),
    framedText(record.canonicalPath),
    uint64Bytes(record.exactSize),
    framedText(record.backend),
    fixedIdentity(record.rootIdentity, FILE_CHECKPOINT_ID_BYTES, 'root identity'),
    fixedIdentity(record.ownedOutputObject, FILE_CHECKPOINT_ID_BYTES, 'owned output object'),
    uint64Bytes(record.stateGeneration),
    uint64Bytes(record.checkpointGeneration),
    uint32Bytes(record.verifiedRanges.length),
    ...record.verifiedRanges.flatMap((range) => [uint64Bytes(range.start), uint64Bytes(range.end)]),
    Uint8Array.of(record.phase),
    Uint8Array.of(record.commitState),
    Uint8Array.of(record.quarantineReason),
    Uint8Array.of(record.quarantineOrigin),
    Uint8Array.of(record.retirementReason),
  ])
}

function checksumFor(record: Omit<FileCheckpointV1, 'checksum'> | FileCheckpointV1): Uint8Array<ArrayBuffer> {
  const payload = canonicalPayload(record as FileCheckpointV1)
  return sha256Sync(concatBytes([new TextEncoder().encode(`${FILE_CHECKPOINT_CHECKSUM_DOMAIN}\0`), payload]))
}

function framedText(value: string): Uint8Array<ArrayBuffer> {
  return framedBytes(new TextEncoder().encode(value))
}

function framedBytes(value: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([uint32Bytes(value.byteLength), value])
}

function uint32Bytes(value: number): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(4)
  new DataView(result.buffer).setUint32(0, value, false)
  return result
}

function uint64Bytes(value: bigint): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(8)
  new DataView(result.buffer).setBigUint64(0, value, false)
  return result
}

function concatBytes(parts: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(parts.reduce((total, part) => total + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result as Uint8Array<ArrayBuffer>
}

function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false
  let difference = 0
  for (let index = 0; index < left.byteLength; index += 1) difference |= (left[index] ?? 0) ^ (right[index] ?? 0)
  return difference === 0
}

function sameRanges(left: readonly FileCheckpointRange[], right: readonly FileCheckpointRange[]): boolean {
  return left.length === right.length && left.every((range, index) => range.start === right[index]!.start && range.end === right[index]!.end)
}

class CheckpointCursor {
  #offset = 0
  readonly #bytes: Uint8Array
  constructor(bytes: Uint8Array) { this.#bytes = bytes }
  byte(): number {
    if (this.#offset >= this.#bytes.byteLength) throw new FileCheckpointError('invalid', 'checkpoint payload is truncated')
    return this.#bytes[this.#offset++]!
  }
  u32(): number {
    const value = this.take(4)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getUint32(0, false)
  }
  u64(): bigint {
    const value = this.take(8)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getBigUint64(0, false)
  }
  text(maxBytes: number): string {
    const length = this.u32()
    if (length > maxBytes) throw new FileCheckpointError('invalid', 'checkpoint text field is too long')
    const bytes = this.take(length)
    try {
      return FILE_CHECKPOINT_TEXT_DECODER.decode(bytes)
    } catch {
      throw new FileCheckpointError('invalid', 'checkpoint text field is invalid UTF-8')
    }
  }
  fixed(length: number, label: string): Uint8Array<ArrayBuffer> {
    const bytes = this.take(length)
    if (bytes.every((byte) => byte === 0)) throw new FileCheckpointError('binding', `${label} must not be zero`)
    return Uint8Array.from(bytes) as Uint8Array<ArrayBuffer>
  }
  take(length: number): Uint8Array {
    if (length < 0 || this.#offset + length > this.#bytes.byteLength) throw new FileCheckpointError('invalid', 'checkpoint payload is truncated')
    const result = this.#bytes.subarray(this.#offset, this.#offset + length)
    this.#offset += length
    return result
  }
  done(): boolean { return this.#offset === this.#bytes.byteLength }
}

// A tiny synchronous SHA-256 keeps record construction deterministic in browser
// storage transactions. WebCrypto remains the authority for the asynchronous
// TransferIntent digest; this local hash only detects accidental journal damage.
function sha256Sync(input: Uint8Array): Uint8Array<ArrayBuffer> {
  const constants = SHA256_CONSTANTS
  const bitLength = BigInt(input.byteLength) * 8n
  const paddedLength = ((input.byteLength + 9 + 63) >> 6) << 6
  const data = new Uint8Array(paddedLength)
  data.set(input)
  data[input.byteLength] = 0x80
  new DataView(data.buffer).setBigUint64(paddedLength - 8, bitLength, false)
  let h0 = 0x6a09e667; let h1 = 0xbb67ae85; let h2 = 0x3c6ef372; let h3 = 0xa54ff53a
  let h4 = 0x510e527f; let h5 = 0x9b05688c; let h6 = 0x1f83d9ab; let h7 = 0x5be0cd19
  const words = new Uint32Array(64)
  for (let offset = 0; offset < data.byteLength; offset += 64) {
    const view = new DataView(data.buffer, offset, 64)
    for (let index = 0; index < 16; index += 1) words[index] = view.getUint32(index * 4, false)
    for (let index = 16; index < 64; index += 1) {
      const x = words[index - 15]!
      const y = words[index - 2]!
      words[index] = (smallSigma1(y) + words[index - 7]! + smallSigma0(x) + words[index - 16]!) >>> 0
    }
    let a = h0; let b = h1; let c = h2; let d = h3; let e = h4; let f = h5; let g = h6; let h = h7
    for (let index = 0; index < 64; index += 1) {
      const t1 = (h + bigSigma1(e) + ((e & f) ^ (~e & g)) + constants[index]! + words[index]!) >>> 0
      const t2 = (bigSigma0(a) + ((a & b) ^ (a & c) ^ (b & c))) >>> 0
      h = g; g = f; f = e; e = (d + t1) >>> 0; d = c; c = b; b = a; a = (t1 + t2) >>> 0
    }
    h0 = (h0 + a) >>> 0; h1 = (h1 + b) >>> 0; h2 = (h2 + c) >>> 0; h3 = (h3 + d) >>> 0
    h4 = (h4 + e) >>> 0; h5 = (h5 + f) >>> 0; h6 = (h6 + g) >>> 0; h7 = (h7 + h) >>> 0
  }
  const output = new Uint8Array(32)
  const view = new DataView(output.buffer)
  ;[h0, h1, h2, h3, h4, h5, h6, h7].forEach((value, index) => view.setUint32(index * 4, value, false))
  return output as Uint8Array<ArrayBuffer>
}

function rotateRight(value: number, count: number): number { return (value >>> count) | (value << (32 - count)) }
function smallSigma0(value: number): number { return rotateRight(value, 7) ^ rotateRight(value, 18) ^ (value >>> 3) }
function smallSigma1(value: number): number { return rotateRight(value, 17) ^ rotateRight(value, 19) ^ (value >>> 10) }
function bigSigma0(value: number): number { return rotateRight(value, 2) ^ rotateRight(value, 13) ^ rotateRight(value, 22) }
function bigSigma1(value: number): number { return rotateRight(value, 6) ^ rotateRight(value, 11) ^ rotateRight(value, 25) }

const SHA256_CONSTANTS = Uint32Array.from([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
])
