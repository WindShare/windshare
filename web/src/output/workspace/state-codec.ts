import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalU8,
  canonicalU64,
  canonicalUnixMilliseconds,
  concatCanonicalBytes,
  type CanonicalBytes,
} from './canonical'
import {
  lifecycleDeadline,
  receiveStateByte,
  type NeedsAttentionReason,
  type ReceiveLifecycleState,
  type RetainedLifecycleKind,
  type RestartRequiredReason,
} from './state'

const RESUMABLE_RECEIVE_FILE_SET = 1
const RESUMABLE_RECEIVE_DIRECT_ZIP = 2
const DIRECT_ZIP_PHASE_BETWEEN_MEMBERS = 1
const DIRECT_ZIP_PHASE_INSIDE_MEMBER = 2
const DIRECT_ZIP_PHASE_CLOSING = 3
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  createPersistedReceiveRecord,
  type PersistedReceiveRecord,
} from './records'

export function canonicalReceiveLifecycleStateBytes(
  state: ReceiveLifecycleState,
): CanonicalBytes {
  if (state.generation === 0n) {
    throw new TypeError('receive lifecycle generation must not be zero')
  }
  return canonicalRecord('windshare/receive-lifecycle-state/v2', 2, [
    canonicalFrame(canonicalIdentity(state.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(state.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalU64(state.generation)),
    canonicalFrame(canonicalU8(receiveStateByte(state))),
    ...lifecyclePayload(state),
  ])
}

export function storedReceiveLifecycleState(
  state: ReceiveLifecycleState,
): Promise<PersistedReceiveRecord> {
  const expiresAt = lifecycleDeadline(state)
  return createPersistedReceiveRecord({
    operationId: state.operationId,
    kind: RECEIVE_RECORD_LIFECYCLE_STATE,
    canonicalBytes: canonicalReceiveLifecycleStateBytes(state),
    state: receiveStateByte(state),
    lifecycleGeneration: state.generation,
    ...(expiresAt === undefined ? {} : { expiresAt }),
  })
}

export function decodeStoredReceiveLifecycleState(
  record: PersistedReceiveRecord,
): ReceiveLifecycleState {
  if (record.kind !== RECEIVE_RECORD_LIFECYCLE_STATE) {
    throw new TypeError('record is not lifecycle authority')
  }
  const state = decodeReceiveLifecycleState(record.canonicalBytes)
  if (state.operationId !== record.operationId ||
      state.generation.toString(10) !== record.lifecycleGeneration ||
      receiveStateByte(state) !== record.state ||
      lifecycleDeadline(state) !== record.expiresAt) {
    throw new TypeError('lifecycle projections disagree with canonical bytes')
  }
  return state
}

function lifecyclePayload(state: ReceiveLifecycleState): readonly CanonicalBytes[] {
  switch (state.kind) {
    case 'intent-frozen': return []
    case 'preparing': return [identityFrame(state.preparationId, 16, 'preparation ID')]
    case 'receiving':
    case 'finalizing-tree':
    case 'committing-atomic':
      return [identityFrame(state.activeLeaseId, 16, 'lease ID')]
    case 'resumable-receive': return resumableReceivePayload(state)
    case 'materialization-sealed':
      return [digestFrame(state.sealedMaterializationDigest, 'sealed materialization digest')]
    case 'packaging': return [
      identityFrame(state.activeLeaseId, 16, 'lease ID'),
      digestFrame(state.sealedMaterializationDigest, 'sealed materialization digest'),
      identityFrame(state.packageTempObjectId, 32, 'package temporary object ID'),
    ]
    case 'resumable-package': return [
      digestFrame(state.sealedMaterializationDigest, 'sealed materialization digest'),
      digestFrame(state.tempCleanupProofDigest, 'temporary cleanup proof digest'),
      millisecondsFrame(state.expiresAt),
    ]
    case 'artifact-sealed':
      return [digestFrame(state.packageDigest, 'package digest')]
    case 'waiting-to-save': return [
      digestFrame(state.packageDigest, 'package digest'),
      millisecondsFrame(state.expiresAt),
    ]
    case 'publishing-managed': return [
      identityFrame(state.activeLeaseId, 16, 'lease ID'),
      digestFrame(state.packageDigest, 'package digest'),
      identityFrame(state.publicationAttemptId, 16, 'publication attempt ID'),
    ]
    case 'handing-off': return handingOffPayload(state)
    case 'published': return [
      digestFrame(state.receiptDigest, 'publication receipt digest'),
      canonicalFrame(canonicalU8(state.cleanupState === 'clean' ? 1 : 2)),
    ]
    case 'download-started': return downloadStartedPayload(state)
    case 'partial-directory': return [
      canonicalFrame(canonicalU8(state.reason === 'failures' ? 1 : 2)),
      canonicalFrame(canonicalU64(state.successCount)),
      canonicalFrame(canonicalU64(state.failureCount)),
      digestFrame(state.receiptDigest, 'partial receipt digest'),
    ]
    case 'restart-required': return [
      canonicalFrame(canonicalU8(restartReasonByte(state.reason))),
      digestFrame(state.receiptDigest, 'rollback receipt digest'),
    ]
    case 'discarded':
      return [digestFrame(state.cleanupReceiptDigest, 'cleanup receipt digest')]
    case 'expired': return [
      canonicalFrame(canonicalU8(stableStateByte(state.priorStableState))),
      millisecondsFrame(state.expiresAt),
      canonicalFrame(canonicalU8(state.cleanupState === 'clean' ? 1 : 2)),
      digestFrame(state.expiryReceiptDigest, 'expiry receipt digest'),
    ]
    case 'needs-attention': return [
      canonicalFrame(canonicalU8(attentionReasonByte(state.reason))),
      digestFrame(state.lastVerifiedRecordDigest, 'last verified record digest'),
    ]
    case 'authorization-required':
    case 'target-verification-required':
    case 'destination-space-required': return [
      digestFrame(state.recoveryGateDigest, 'recovery gate digest'),
      millisecondsFrame(state.expiresAt),
    ]
  }
}

function resumableReceivePayload(
  state: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
): readonly CanonicalBytes[] {
  if (state.payloadKind === 'direct-zip') {
    return [
      canonicalFrame(canonicalU8(RESUMABLE_RECEIVE_DIRECT_ZIP)),
      digestFrame(state.directZipCheckpointDigest, 'direct ZIP checkpoint digest'),
      canonicalFrame(canonicalU64(state.safeSelectedPayloadBytes)),
      canonicalFrame(canonicalU64(state.committedArchiveLength)),
      canonicalFrame(canonicalU8(directZipCheckpointPhaseByte(state.checkpointPhase))),
      millisecondsFrame(state.expiresAt),
    ]
  }
  return [
    canonicalFrame(canonicalU8(RESUMABLE_RECEIVE_FILE_SET)),
    digestFrame(state.checkpointSetDigest, 'checkpoint set digest'),
    canonicalFrame(canonicalU64(state.completedFileCount)),
    canonicalFrame(canonicalU64(state.completedBytes)),
    millisecondsFrame(state.expiresAt),
    canonicalFrame(state.partialReceiptDigest === undefined
      ? canonicalU8(1)
      : concatCanonicalBytes([
          canonicalU8(2),
          digestFrame(state.partialReceiptDigest, 'partial receipt digest'),
        ])),
  ]
}

function handingOffPayload(
  state: Extract<ReceiveLifecycleState, { kind: 'handing-off' }>,
): readonly CanonicalBytes[] {
  const workspace = state.attemptKind === 'workspace'
  return [
    identityFrame(state.activeLeaseId, 16, 'lease ID'),
    canonicalFrame(canonicalU8(workspace ? 1 : 2)),
    identityFrame(state.attemptId, 16, 'handoff attempt ID'),
    optionalDigestFrame(workspace ? state.packageDigest : undefined, 'package digest'),
    optionalMillisecondsFrame(workspace ? state.retainedDeadline : undefined),
  ]
}

function downloadStartedPayload(
  state: Extract<ReceiveLifecycleState, { kind: 'download-started' }>,
): readonly CanonicalBytes[] {
  const workspace = state.attemptKind === 'workspace'
  return [
    canonicalFrame(canonicalU8(workspace ? 1 : 2)),
    identityFrame(state.attemptId, 16, 'handoff attempt ID'),
    optionalDigestFrame(workspace ? state.packageDigest : undefined, 'package digest'),
    optionalMillisecondsFrame(workspace ? state.retryableUntil : undefined),
  ]
}

function optionalDigestFrame(value: string | undefined, label: string): CanonicalBytes {
  return canonicalFrame(value === undefined
    ? canonicalU8(1)
    : concatCanonicalBytes([canonicalU8(2), digestFrame(value, label)]))
}

function optionalMillisecondsFrame(value: number | undefined): CanonicalBytes {
  return canonicalFrame(value === undefined
    ? canonicalU8(1)
    : concatCanonicalBytes([canonicalU8(2), millisecondsFrame(value)]))
}

function identityFrame(value: string, width: number, label: string): CanonicalBytes {
  return canonicalFrame(canonicalIdentity(value, width, label))
}

function digestFrame(value: string, label: string): CanonicalBytes {
  return identityFrame(value, 32, label)
}

function millisecondsFrame(value: number): CanonicalBytes {
  return canonicalFrame(canonicalUnixMilliseconds(value))
}

function attentionReasonByte(reason: NeedsAttentionReason): number {
  switch (reason) {
    case 'target-ownership-unknown': return 1
    case 'publication-unknown': return 2
    case 'cleanup-unknown': return 3
  }
}

function restartReasonByte(reason: RestartRequiredReason): number {
  switch (reason) {
    case 'direct-atomic-rolled-back': return 1
    case 'portable-aborted': return 2
    case 'source-revision-changed': return 3
    case 'preparation-invalidated': return 4
    case 'content-session-ended': return 5
    case 'target-deleted': return 6
  }
}

function stableStateByte(
  state: RetainedLifecycleKind,
): number {
  switch (state) {
    case 'resumable-receive': return 4
    case 'resumable-package': return 9
    case 'waiting-to-save': return 11
    case 'download-started': return 15
    case 'authorization-required': return 21
    case 'target-verification-required': return 22
    case 'destination-space-required': return 23
  }
}

interface DecodedLifecycleBase {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly generation: bigint
}

export function decodeReceiveLifecycleState(bytes: Uint8Array): ReceiveLifecycleState {
  const reader = LifecycleFrameReader.open(bytes)
  const operationId = reader.identity(16, 'operation ID')
  const receiveIntentDigest = reader.identity(32, 'receive intent digest')
  const generation = reader.u64('lifecycle generation')
  if (generation === 0n) throw new TypeError('receive lifecycle generation must not be zero')
  const base: DecodedLifecycleBase = { operationId, receiveIntentDigest, generation }
  const stateByte = reader.byte('lifecycle state')
  const state = stateByte <= 10
    ? decodeMaterializationState(reader, base, stateByte)
    : decodePublicationState(reader, base, stateByte)
  reader.finish()
  if (!equalBytes(canonicalReceiveLifecycleStateBytes(state), bytes)) {
    throw new TypeError('receive lifecycle state is not canonically encoded')
  }
  return state
}

function decodeMaterializationState(
  reader: LifecycleFrameReader,
  base: DecodedLifecycleBase,
  stateByte: number,
): ReceiveLifecycleState {
  switch (stateByte) {
    case 1: return Object.freeze({ ...base, kind: 'intent-frozen' })
    case 2: return Object.freeze({
      ...base,
      kind: 'preparing',
      preparationId: reader.identity(16, 'preparation ID'),
    })
    case 3: return Object.freeze({
      ...base,
      kind: 'receiving',
      activeLeaseId: reader.identity(16, 'lease ID'),
    })
    case 4: return decodeResumableReceiveState(reader, base)
    case 5: return Object.freeze({
      ...base,
      kind: 'finalizing-tree',
      activeLeaseId: reader.identity(16, 'lease ID'),
    })
    case 6: return Object.freeze({
      ...base,
      kind: 'committing-atomic',
      activeLeaseId: reader.identity(16, 'lease ID'),
    })
    case 7: return Object.freeze({
      ...base,
      kind: 'materialization-sealed',
      sealedMaterializationDigest: reader.identity(32, 'sealed materialization digest'),
    })
    case 8: return Object.freeze({
      ...base,
      kind: 'packaging',
      activeLeaseId: reader.identity(16, 'lease ID'),
      sealedMaterializationDigest: reader.identity(32, 'sealed materialization digest'),
      packageTempObjectId: reader.identity(32, 'package temporary object ID'),
    })
    case 9: return Object.freeze({
      ...base,
      kind: 'resumable-package',
      sealedMaterializationDigest: reader.identity(32, 'sealed materialization digest'),
      tempCleanupProofDigest: reader.identity(32, 'temporary cleanup proof digest'),
      expiresAt: reader.milliseconds(),
    })
    case 10: return Object.freeze({
      ...base,
      kind: 'artifact-sealed',
      packageDigest: reader.identity(32, 'package digest'),
    })
    default: throw new TypeError('receive lifecycle state discriminant is invalid')
  }
}

function decodePublicationState(
  reader: LifecycleFrameReader,
  base: DecodedLifecycleBase,
  stateByte: number,
): ReceiveLifecycleState {
  switch (stateByte) {
    case 11: return Object.freeze({
      ...base,
      kind: 'waiting-to-save',
      packageDigest: reader.identity(32, 'package digest'),
      expiresAt: reader.milliseconds(),
    })
    case 12: return Object.freeze({
      ...base,
      kind: 'publishing-managed',
      activeLeaseId: reader.identity(16, 'lease ID'),
      packageDigest: reader.identity(32, 'package digest'),
      publicationAttemptId: reader.identity(16, 'publication attempt ID'),
    })
    case 13: return decodeHandingOffState(reader, base)
    case 14: return Object.freeze({
      ...base,
      kind: 'published',
      receiptDigest: reader.identity(32, 'publication receipt digest'),
      cleanupState: cleanupStateFromByte(reader.byte('cleanup state')),
    })
    case 15: return decodeDownloadStartedState(reader, base)
    case 16: return Object.freeze({
      ...base,
      kind: 'partial-directory',
      reason: partialReasonFromByte(reader.byte('partial directory reason')),
      successCount: reader.u64('successful file count'),
      failureCount: reader.u64('failed file count'),
      receiptDigest: reader.identity(32, 'partial receipt digest'),
    })
    case 17: return Object.freeze({
      ...base,
      kind: 'restart-required',
      reason: restartReasonFromByte(reader.byte('restart reason')),
      receiptDigest: reader.identity(32, 'rollback receipt digest'),
    })
    case 18: return Object.freeze({
      ...base,
      kind: 'discarded',
      cleanupReceiptDigest: reader.identity(32, 'cleanup receipt digest'),
    })
    case 19: return Object.freeze({
      ...base,
      kind: 'expired',
      priorStableState: stableStateFromByte(reader.byte('prior stable state')),
      expiresAt: reader.milliseconds(),
      cleanupState: cleanupStateFromByte(reader.byte('cleanup state')),
      expiryReceiptDigest: reader.identity(32, 'expiry receipt digest'),
    })
    case 20: return Object.freeze({
      ...base,
      kind: 'needs-attention',
      reason: attentionReasonFromByte(reader.byte('attention reason')),
      lastVerifiedRecordDigest: reader.identity(32, 'last verified record digest'),
    })
    case 21: return decodeRecoveryGateState(reader, base, 'authorization-required')
    case 22: return decodeRecoveryGateState(reader, base, 'target-verification-required')
    case 23: return decodeRecoveryGateState(reader, base, 'destination-space-required')
    default: throw new TypeError('receive lifecycle state discriminant is invalid')
  }
}

function decodeResumableReceiveState(
  reader: LifecycleFrameReader,
  base: DecodedLifecycleBase,
): ReceiveLifecycleState {
  const payloadKind = reader.byte('resumable receive payload kind')
  if (payloadKind === RESUMABLE_RECEIVE_FILE_SET) {
    return Object.freeze({
      ...base,
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: reader.identity(32, 'checkpoint set digest'),
      completedFileCount: reader.u64('completed file count'),
      completedBytes: reader.u64('completed bytes'),
      expiresAt: reader.milliseconds(),
      ...optionalDigest(reader.frame(), 'partial receipt digest', 'partialReceiptDigest'),
    })
  }
  if (payloadKind === RESUMABLE_RECEIVE_DIRECT_ZIP) {
    return Object.freeze({
      ...base,
      kind: 'resumable-receive',
      payloadKind: 'direct-zip',
      directZipCheckpointDigest: reader.identity(32, 'direct ZIP checkpoint digest'),
      safeSelectedPayloadBytes: reader.u64('safe selected payload bytes'),
      committedArchiveLength: reader.u64('committed archive length'),
      checkpointPhase: directZipCheckpointPhaseFromByte(reader.byte('direct ZIP checkpoint phase')),
      expiresAt: reader.milliseconds(),
    })
  }
  throw new TypeError('resumable receive payload kind is invalid')
}

function decodeRecoveryGateState(
  reader: LifecycleFrameReader,
  base: DecodedLifecycleBase,
  kind: 'authorization-required' | 'target-verification-required' | 'destination-space-required',
): ReceiveLifecycleState {
  return Object.freeze({
    ...base,
    kind,
    recoveryGateDigest: reader.identity(32, 'recovery gate digest'),
    expiresAt: reader.milliseconds(),
  })
}

function decodeHandingOffState(
  reader: LifecycleFrameReader,
  base: DecodedLifecycleBase,
): ReceiveLifecycleState {
  const activeLeaseId = reader.identity(16, 'lease ID')
  const attemptKind = attemptKindFromByte(reader.byte('handoff attempt kind'))
  const attemptId = reader.identity(16, 'handoff attempt ID')
  const packageDigest = optionalDigestValue(reader.frame(), 'package digest')
  const retainedDeadline = optionalMillisecondsValue(reader.frame())
  if (attemptKind === 'workspace') {
    if (packageDigest === undefined || retainedDeadline === undefined) {
      throw new TypeError('workspace handoff must retain its package and deadline')
    }
    return Object.freeze({
      ...base,
      kind: 'handing-off',
      activeLeaseId,
      attemptKind,
      attemptId,
      packageDigest,
      retainedDeadline,
    })
  }
  if (packageDigest !== undefined || retainedDeadline !== undefined) {
    throw new TypeError('portable handoff cannot persist workspace retention authority')
  }
  return Object.freeze({ ...base, kind: 'handing-off', activeLeaseId, attemptKind, attemptId })
}

function decodeDownloadStartedState(
  reader: LifecycleFrameReader,
  base: DecodedLifecycleBase,
): ReceiveLifecycleState {
  const attemptKind = attemptKindFromByte(reader.byte('handoff attempt kind'))
  const attemptId = reader.identity(16, 'handoff attempt ID')
  const packageDigest = optionalDigestValue(reader.frame(), 'package digest')
  const retryableUntil = optionalMillisecondsValue(reader.frame())
  if (attemptKind === 'workspace') {
    if (packageDigest === undefined || retryableUntil === undefined) {
      throw new TypeError('workspace download must retain its package and deadline')
    }
    return Object.freeze({
      ...base,
      kind: 'download-started',
      attemptKind,
      attemptId,
      packageDigest,
      retryableUntil,
    })
  }
  if (packageDigest !== undefined || retryableUntil !== undefined) {
    throw new TypeError('portable download cannot persist workspace retention authority')
  }
  return Object.freeze({ ...base, kind: 'download-started', attemptKind, attemptId })
}

class LifecycleFrameReader {
  readonly #bytes: Uint8Array
  #offset: number

  private constructor(bytes: Uint8Array, offset: number) {
    this.#bytes = bytes
    this.#offset = offset
  }

  static open(bytes: Uint8Array): LifecycleFrameReader {
    if (!(bytes instanceof Uint8Array)) throw new TypeError('lifecycle record must be bytes')
    const prefix = new TextEncoder().encode('windshare/receive-lifecycle-state/v2\0')
    if (bytes.byteLength <= prefix.byteLength ||
        !equalBytes(bytes.subarray(0, prefix.byteLength), prefix) ||
        bytes[prefix.byteLength] !== 2) {
      throw new TypeError('lifecycle record domain or version is invalid')
    }
    return new LifecycleFrameReader(bytes, prefix.byteLength + 1)
  }

  frame(): Uint8Array {
    if (this.#bytes.byteLength - this.#offset < 8) {
      throw new TypeError('lifecycle field frame is truncated')
    }
    const length = new DataView(
      this.#bytes.buffer,
      this.#bytes.byteOffset + this.#offset,
      8,
    ).getBigUint64(0, false)
    this.#offset += 8
    if (length > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new TypeError('lifecycle field frame exceeds the safe bound')
    }
    const end = this.#offset + Number(length)
    if (end > this.#bytes.byteLength) throw new TypeError('lifecycle field frame is truncated')
    const value = this.#bytes.subarray(this.#offset, end)
    this.#offset = end
    return value
  }

  identity(width: number, label: string): string {
    const value = this.frame()
    if (value.byteLength !== width) throw new TypeError(`${label} width is invalid`)
    return encodeBase64Url(canonicalIdentity(encodeBase64Url(value), width, label))
  }

  byte(label: string): number {
    const value = this.frame()
    if (value.byteLength !== 1 || value[0] === undefined) {
      throw new TypeError(`${label} must be a framed byte`)
    }
    return value[0]
  }

  u64(label: string): bigint {
    const value = this.frame()
    if (value.byteLength !== 8) throw new TypeError(`${label} must be a framed u64`)
    return new DataView(value.buffer, value.byteOffset, 8).getBigUint64(0, false)
  }

  milliseconds(): number {
    const value = this.u64('lifecycle deadline')
    if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new TypeError('lifecycle deadline exceeds the safe integer bound')
    }
    return Number(value)
  }

  finish(): void {
    if (this.#offset !== this.#bytes.byteLength) {
      throw new TypeError('lifecycle record contains trailing fields')
    }
  }
}

function optionalDigest<K extends string>(
  value: Uint8Array,
  label: string,
  key: K,
): Readonly<Record<K, string>> | Readonly<Record<string, never>> {
  const present = optionalDigestValue(value, label)
  return present === undefined
    ? Object.freeze({})
    : Object.freeze({ [key]: present }) as Readonly<Record<K, string>>
}

function optionalDigestValue(value: Uint8Array, label: string): string | undefined {
  const present = optionalUnionPayload(value, label)
  return present === undefined
    ? undefined
    : encodeBase64Url(canonicalIdentity(encodeBase64Url(present), 32, label))
}

function optionalMillisecondsValue(value: Uint8Array): number | undefined {
  const present = optionalUnionPayload(value, 'optional lifecycle deadline')
  if (present === undefined) return undefined
  if (present.byteLength !== 8) throw new TypeError('optional lifecycle deadline is not a u64')
  const decoded = new DataView(present.buffer, present.byteOffset, 8).getBigUint64(0, false)
  if (decoded > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new TypeError('optional lifecycle deadline exceeds the safe integer bound')
  }
  return Number(decoded)
}

function optionalUnionPayload(value: Uint8Array, label: string): Uint8Array | undefined {
  if (value[0] === 1 && value.byteLength === 1) return undefined
  if (value[0] !== 2 || value.byteLength < 9) throw new TypeError(`${label} union is invalid`)
  const length = new DataView(value.buffer, value.byteOffset + 1, 8).getBigUint64(0, false)
  if (length > BigInt(Number.MAX_SAFE_INTEGER) ||
      Number(length) !== value.byteLength - 9) {
    throw new TypeError(`${label} union frame is invalid`)
  }
  return value.subarray(9)
}

function attemptKindFromByte(value: number): 'workspace' | 'portable' {
  if (value === 1) return 'workspace'
  if (value === 2) return 'portable'
  throw new TypeError('handoff attempt kind is invalid')
}

function cleanupStateFromByte(value: number): 'clean' | 'cleanup-pending' {
  if (value === 1) return 'clean'
  if (value === 2) return 'cleanup-pending'
  throw new TypeError('cleanup state is invalid')
}

function partialReasonFromByte(value: number): 'failures' | 'stopped' {
  if (value === 1) return 'failures'
  if (value === 2) return 'stopped'
  throw new TypeError('partial directory reason is invalid')
}

function restartReasonFromByte(value: number): RestartRequiredReason {
  const reasons: readonly RestartRequiredReason[] = [
    'direct-atomic-rolled-back',
    'portable-aborted',
    'source-revision-changed',
    'preparation-invalidated',
    'content-session-ended',
    'target-deleted',
  ]
  const reason = reasons[value - 1]
  if (reason === undefined) throw new TypeError('restart reason is invalid')
  return reason
}

function stableStateFromByte(
  value: number,
): RetainedLifecycleKind {
  switch (value) {
    case 4: return 'resumable-receive'
    case 9: return 'resumable-package'
    case 11: return 'waiting-to-save'
    case 15: return 'download-started'
    case 21: return 'authorization-required'
    case 22: return 'target-verification-required'
    case 23: return 'destination-space-required'
    default: throw new TypeError('prior stable state is invalid')
  }
}

function directZipCheckpointPhaseByte(
  phase: 'between-members' | 'inside-member' | 'closing',
): number {
  switch (phase) {
    case 'between-members': return DIRECT_ZIP_PHASE_BETWEEN_MEMBERS
    case 'inside-member': return DIRECT_ZIP_PHASE_INSIDE_MEMBER
    case 'closing': return DIRECT_ZIP_PHASE_CLOSING
  }
}

function directZipCheckpointPhaseFromByte(
  value: number,
): 'between-members' | 'inside-member' | 'closing' {
  switch (value) {
    case DIRECT_ZIP_PHASE_BETWEEN_MEMBERS: return 'between-members'
    case DIRECT_ZIP_PHASE_INSIDE_MEMBER: return 'inside-member'
    case DIRECT_ZIP_PHASE_CLOSING: return 'closing'
    default: throw new TypeError('direct ZIP checkpoint phase is invalid')
  }
}

function attentionReasonFromByte(value: number): NeedsAttentionReason {
  if (value === 1) return 'target-ownership-unknown'
  if (value === 2) return 'publication-unknown'
  if (value === 3) return 'cleanup-unknown'
  throw new TypeError('attention reason is invalid')
}
