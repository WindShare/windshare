import type { DirectZipEntryPlanV2 } from '../format'

export const DIRECT_ZIP_WRITER_CHECKPOINT_VERSION = 1 as const
export const DIRECT_ZIP_WRITER_CANDIDATE_VERSION = 1 as const

export type DirectZipWriterPhase = 'between-members' | 'inside-member' | 'closing'

export interface DirectZipWriterPageStateV1 {
  readonly layoutRoot: Uint8Array
  readonly layoutRecordCount: bigint
  readonly centralRoot: Uint8Array
  readonly centralRecordCount: bigint
  readonly centralBytes: bigint
}

export interface DirectZipSourceAuthorityV1 {
  readonly fileId: string
  readonly revision: string
  readonly exactSize: bigint
  readonly rangeAuthority: string
}

export interface DirectZipMemberRollbackV1 {
  readonly archiveOffset: bigint
  readonly safeResumeBytes: bigint
  readonly nextEntryOrdinal: bigint
  readonly epochStart: bigint
  readonly predecessorEpochRoot: Uint8Array
  readonly epochContentDigest: Uint8Array
  readonly epochRoot: Uint8Array
  readonly pages: DirectZipWriterPageStateV1
}

export interface DirectZipActiveMemberV1 {
  readonly plan: DirectZipEntryPlanV2
  readonly source: DirectZipSourceAuthorityV1
  readonly payloadOffset: bigint
  readonly crc32Accumulator: number
  readonly rollback: DirectZipMemberRollbackV1
}

export interface DirectZipClosingStateV1 {
  readonly centralDirectoryOffset: bigint
  readonly centralDirectoryBytes: bigint
  readonly replayStartOrdinal: bigint
}

export interface DirectZipCommittedCompletionV1 {
  readonly exactArchiveBytes: bigint
  readonly preClosingEpochRoot: Uint8Array
}

export interface DirectZipWriterCheckpointV1 {
  readonly version: typeof DIRECT_ZIP_WRITER_CHECKPOINT_VERSION
  readonly operationId: string
  readonly intentDigest: Uint8Array
  readonly generation: bigint
  readonly phase: DirectZipWriterPhase
  readonly nextEntryOrdinal: bigint
  readonly archiveOffset: bigint
  readonly committedLength: bigint
  readonly safeResumeBytes: bigint
  readonly targetObservationDigest: Uint8Array
  readonly epochRoot: Uint8Array
  readonly pages: DirectZipWriterPageStateV1
  readonly member?: DirectZipActiveMemberV1
  readonly closing?: DirectZipClosingStateV1
  readonly completion?: DirectZipCommittedCompletionV1
}

export interface DirectZipEpochCandidateV1 {
  readonly version: typeof DIRECT_ZIP_WRITER_CANDIDATE_VERSION
  readonly kind: 'epoch' | 'closing'
  readonly candidateId: string
  readonly epochId: string
  readonly operationId: string
  readonly predecessorGeneration: bigint
  readonly predecessorLength: bigint
  readonly predecessorObservationDigest: Uint8Array
  readonly rangeStart: bigint
  readonly stagedEnd: bigint
  readonly contentDigest: Uint8Array
  readonly expectedEpochRoot: Uint8Array
  readonly proposed: DirectZipWriterCheckpointV1
}

export interface DirectZipEpochProofV1 {
  readonly start: bigint
  readonly end: bigint
  readonly contentDigest: Uint8Array
  readonly predecessorRoot: Uint8Array
  readonly epochRoot: Uint8Array
}

export interface DirectZipCompletionSealV1 {
  readonly entryCount: bigint
  readonly centralDirectoryBytes: bigint
  readonly layoutRoot: Uint8Array
  readonly centralRoot: Uint8Array
  readonly preClosingEpochRoot: Uint8Array
}

export interface DirectZipCompletionProofV1 {
  readonly checkpoint: DirectZipWriterCheckpointV1
  readonly exactArchiveBytes: bigint
  readonly finalEpochRoot: Uint8Array
  readonly targetObservationDigest: Uint8Array
}

export interface DirectZipMemberAdmissionV1 {
  readonly plan: DirectZipEntryPlanV2
  readonly layoutEvidence: Uint8Array
  readonly discoveryEvidence: Uint8Array
  readonly source?: DirectZipSourceAuthorityV1
}

export type DirectZipMemberResumeDecisionV1 =
  | Readonly<{
      kind: 'resume'
      payloadOffset: bigint
      crc32Accumulator: number
    }>
  | Readonly<{
      kind: 'rollback-member'
      archiveOffset: bigint
      safeResumeBytes: bigint
      nextEntryOrdinal: bigint
      reason: 'file-id-changed' | 'revision-changed' | 'size-changed' | 'range-authority-changed'
    }>

type DirectZipChangedSourceReasonV1 = Extract<
  DirectZipMemberResumeDecisionV1,
  { readonly kind: 'rollback-member' }
>['reason']

export function decideDirectZipMemberResumeV1(
  checkpoint: DirectZipWriterCheckpointV1,
  source: DirectZipSourceAuthorityV1,
): DirectZipMemberResumeDecisionV1 {
  if (checkpoint.phase !== 'inside-member' || checkpoint.member === undefined) {
    throw new Error('direct ZIP checkpoint is not inside a member')
  }
  const current = checkpoint.member.source
  const reason = changedSourceReason(current, source)
  if (reason === undefined) {
    return Object.freeze({
      kind: 'resume',
      payloadOffset: checkpoint.member.payloadOffset,
      crc32Accumulator: checkpoint.member.crc32Accumulator,
    })
  }
  const rollback = checkpoint.member.rollback
  return Object.freeze({
    kind: 'rollback-member',
    archiveOffset: rollback.archiveOffset,
    safeResumeBytes: rollback.safeResumeBytes,
    nextEntryOrdinal: rollback.nextEntryOrdinal,
    reason,
  })
}

function changedSourceReason(
  current: DirectZipSourceAuthorityV1,
  source: DirectZipSourceAuthorityV1,
): DirectZipChangedSourceReasonV1 | undefined {
  if (current.fileId !== source.fileId) return 'file-id-changed'
  if (current.revision !== source.revision) return 'revision-changed'
  if (current.exactSize !== source.exactSize) return 'size-changed'
  if (current.rangeAuthority !== source.rangeAuthority) return 'range-authority-changed'
  return undefined
}
