import {
  DIRECT_ZIP_SHA256_BYTES,
  chainDirectZipEpochDigestV1,
  planDirectZipClosingLayoutV2,
  requireDirectZipFsaOffset,
  validateDirectZipEntryPlanV2,
  type DirectZipEntryPlanV2,
} from '../format'
import { equalDirectZipBytes } from '../format/canonical'
import { checkedZipAdd } from '../../zip-layout/policy'
import {
  DIRECT_ZIP_WRITER_CHECKPOINT_VERSION,
  type DirectZipActiveMemberV1,
  type DirectZipCommittedCompletionV1,
  type DirectZipCompletionSealV1,
  type DirectZipSourceAuthorityV1,
  type DirectZipWriterCheckpointV1,
  type DirectZipWriterPageStateV1,
} from './model'

export interface MutableDirectZipMemberState {
  readonly plan: DirectZipEntryPlanV2
  readonly source: DirectZipSourceAuthorityV1
  readonly rollback: DirectZipActiveMemberV1['rollback']
  payloadOffset: bigint
  crc32Accumulator: number
}

export function snapshotCheckpoint(
  checkpoint: DirectZipWriterCheckpointV1,
): DirectZipWriterCheckpointV1 {
  return Object.freeze({
    ...checkpoint,
    intentDigest: Uint8Array.from(checkpoint.intentDigest),
    targetObservationDigest: Uint8Array.from(checkpoint.targetObservationDigest),
    epochRoot: Uint8Array.from(checkpoint.epochRoot),
    pages: snapshotPageState(checkpoint.pages),
    ...(checkpoint.member === undefined ? {} : { member: snapshotMember(checkpoint.member) }),
    ...(checkpoint.closing === undefined ? {} : { closing: { ...checkpoint.closing } }),
    ...(checkpoint.completion === undefined
      ? {}
      : { completion: snapshotCompletion(checkpoint.completion) }),
  })
}

export function snapshotPageState(
  state: DirectZipWriterPageStateV1,
): DirectZipWriterPageStateV1 {
  return Object.freeze({
    ...state,
    layoutRoot: Uint8Array.from(state.layoutRoot),
    centralRoot: Uint8Array.from(state.centralRoot),
  })
}

export function snapshotSource(
  source: DirectZipSourceAuthorityV1,
): DirectZipSourceAuthorityV1 {
  return Object.freeze({ ...source })
}

export function snapshotMutableMember(
  member: MutableDirectZipMemberState,
): DirectZipActiveMemberV1 {
  return snapshotMember({ ...member })
}

export function mutableMember(
  member: DirectZipActiveMemberV1 | undefined,
): MutableDirectZipMemberState | undefined {
  if (member === undefined) return undefined
  const snapshot = snapshotMember(member)
  return {
    plan: snapshot.plan,
    source: snapshot.source,
    rollback: snapshot.rollback,
    payloadOffset: snapshot.payloadOffset,
    crc32Accumulator: snapshot.crc32Accumulator,
  }
}

export function checkpointWithObservation(
  checkpoint: DirectZipWriterCheckpointV1,
  observationDigest: Uint8Array,
): DirectZipWriterCheckpointV1 {
  return snapshotCheckpoint(Object.freeze({
    ...checkpoint,
    targetObservationDigest: Uint8Array.from(observationDigest),
  }))
}

export function checkpointWithCompletion(
  checkpoint: DirectZipWriterCheckpointV1,
  completion: DirectZipCommittedCompletionV1,
): DirectZipWriterCheckpointV1 {
  return snapshotCheckpoint(Object.freeze({
    ...checkpoint,
    completion: snapshotCompletion(completion),
  }))
}

export function requireCheckpointShape(checkpoint: DirectZipWriterCheckpointV1): void {
  requireDirectZipFsaOffset(checkpoint.archiveOffset, 'direct ZIP checkpoint archive offset')
  requireDirectZipFsaOffset(checkpoint.committedLength, 'direct ZIP checkpoint committed length')
  requireDirectZipFsaOffset(checkpoint.safeResumeBytes, 'direct ZIP checkpoint safe source bytes')
  if (checkpoint.version !== DIRECT_ZIP_WRITER_CHECKPOINT_VERSION ||
      checkpoint.operationId.length === 0 || checkpoint.generation < 0n ||
      checkpoint.archiveOffset !== checkpoint.committedLength ||
      checkpoint.intentDigest.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      checkpoint.epochRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      checkpoint.targetObservationDigest.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      checkpoint.pages.layoutRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      checkpoint.pages.centralRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      checkpoint.pages.layoutRecordCount < 1n || checkpoint.pages.centralRecordCount < 1n ||
      checkpoint.pages.centralBytes < 1n) {
    throw new TypeError('direct ZIP writer checkpoint is invalid')
  }
  if ((checkpoint.phase === 'inside-member') !== (checkpoint.member !== undefined) ||
      (checkpoint.phase === 'closing') !== (checkpoint.closing !== undefined) ||
      (checkpoint.completion !== undefined && checkpoint.phase !== 'closing')) {
    throw new TypeError('direct ZIP writer checkpoint phase payload is invalid')
  }
  const admittedMembers = checkpoint.phase === 'inside-member'
    ? checkpoint.nextEntryOrdinal + 1n
    : checkpoint.nextEntryOrdinal
  if (checkpoint.pages.layoutRecordCount !== admittedMembers ||
      checkpoint.pages.centralRecordCount !== checkpoint.nextEntryOrdinal) {
    throw new TypeError('direct ZIP checkpoint page counts disagree with its member phase')
  }
  if (checkpoint.member !== undefined) requireInsideMemberCheckpoint(checkpoint, checkpoint.member)
  if (checkpoint.closing !== undefined) requireClosingCheckpoint(checkpoint)
  if (checkpoint.completion !== undefined) requireCompletionCheckpoint(checkpoint)
}

export function requireSourceMatchesPlan(
  source: DirectZipSourceAuthorityV1,
  plan: DirectZipEntryPlanV2,
): void {
  if (source.fileId.length === 0 || source.revision.length === 0 ||
      source.rangeAuthority.length === 0 || source.exactSize !== plan.zipEntry.exactSize) {
    throw new TypeError('direct ZIP source authority disagrees with the member plan')
  }
}

export function requireCompletionSeal(
  seal: DirectZipCompletionSealV1,
  pages: DirectZipWriterPageStateV1,
  entryCount: bigint,
  epochRoot: Uint8Array,
): void {
  if (seal.entryCount !== entryCount || seal.entryCount !== pages.layoutRecordCount ||
      seal.entryCount !== pages.centralRecordCount ||
      seal.centralDirectoryBytes !== pages.centralBytes ||
      !equalDirectZipBytes(seal.layoutRoot, pages.layoutRoot) ||
      !equalDirectZipBytes(seal.centralRoot, pages.centralRoot) ||
      !equalDirectZipBytes(seal.preClosingEpochRoot, epochRoot)) {
    throw new Error('direct ZIP completion seal disagrees with durable page roots')
  }
}

function snapshotMember(member: DirectZipActiveMemberV1): DirectZipActiveMemberV1 {
  return Object.freeze({
    ...member,
    plan: validateDirectZipEntryPlanV2(member.plan),
    source: snapshotSource(member.source),
    rollback: Object.freeze({
      ...member.rollback,
      predecessorEpochRoot: Uint8Array.from(member.rollback.predecessorEpochRoot),
      epochContentDigest: Uint8Array.from(member.rollback.epochContentDigest),
      epochRoot: Uint8Array.from(member.rollback.epochRoot),
      pages: snapshotPageState(member.rollback.pages),
    }),
  })
}

function snapshotCompletion(
  completion: DirectZipCommittedCompletionV1,
): DirectZipCommittedCompletionV1 {
  return Object.freeze({
    exactArchiveBytes: completion.exactArchiveBytes,
    preClosingEpochRoot: Uint8Array.from(completion.preClosingEpochRoot),
  })
}

function requireInsideMemberCheckpoint(
  checkpoint: DirectZipWriterCheckpointV1,
  member: DirectZipActiveMemberV1,
): void {
  const expectedOffset = checkedZipAdd(
    member.plan.zipEntry.localHeaderOffset,
    member.plan.localHeaderBytes,
    member.payloadOffset,
  )
  const rollback = member.rollback
  validateDirectZipEntryPlanV2(member.plan)
  requireSourceMatchesPlan(member.source, member.plan)
  const rollbackRoot = rollback.archiveOffset === rollback.epochStart
    ? rollback.predecessorEpochRoot
    : chainDirectZipEpochDigestV1({
        predecessorRoot: rollback.predecessorEpochRoot,
        start: rollback.epochStart,
        end: rollback.archiveOffset,
        contentDigest: rollback.epochContentDigest,
      })
  if (member.plan.ordinal !== checkpoint.nextEntryOrdinal ||
      member.payloadOffset > member.source.exactSize ||
      member.source.exactSize !== member.plan.zipEntry.exactSize ||
      expectedOffset !== checkpoint.archiveOffset ||
      member.rollback.archiveOffset !== member.plan.zipEntry.localHeaderOffset ||
      member.rollback.nextEntryOrdinal !== checkpoint.nextEntryOrdinal ||
      member.rollback.pages.layoutRecordCount !== checkpoint.nextEntryOrdinal ||
      member.rollback.pages.centralRecordCount !== checkpoint.nextEntryOrdinal ||
      member.rollback.pages.layoutRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      member.rollback.pages.centralRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      member.rollback.safeResumeBytes + member.payloadOffset !== checkpoint.safeResumeBytes ||
      !Number.isInteger(member.crc32Accumulator) || member.crc32Accumulator < 0 ||
      member.crc32Accumulator > 0xffff_ffff ||
      rollback.epochStart > rollback.archiveOffset ||
      rollback.predecessorEpochRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      rollback.epochContentDigest.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      rollback.epochRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES ||
      !equalDirectZipBytes(rollbackRoot, rollback.epochRoot)) {
    throw new TypeError('direct ZIP inside-member checkpoint coordinates are inconsistent')
  }
}

function requireCompletionCheckpoint(checkpoint: DirectZipWriterCheckpointV1): void {
  const completion = checkpoint.completion!
  const closing = checkpoint.closing!
  const layout = planDirectZipClosingLayoutV2({
    entryCount: checkpoint.nextEntryOrdinal,
    centralDirectoryOffset: closing.centralDirectoryOffset,
    centralDirectoryBytes: closing.centralDirectoryBytes,
  })
  if (completion.exactArchiveBytes !== checkpoint.committedLength ||
      completion.exactArchiveBytes !== layout.exactArchiveBytes ||
      completion.preClosingEpochRoot.byteLength !== DIRECT_ZIP_SHA256_BYTES) {
    throw new TypeError('direct ZIP committed completion authority is inconsistent')
  }
}

function requireClosingCheckpoint(checkpoint: DirectZipWriterCheckpointV1): void {
  const closing = checkpoint.closing!
  if (closing.replayStartOrdinal !== 0n ||
      closing.centralDirectoryOffset > checkpoint.archiveOffset ||
      closing.centralDirectoryBytes !== checkpoint.pages.centralBytes) {
    throw new TypeError('direct ZIP closing checkpoint coordinates are inconsistent')
  }
}
