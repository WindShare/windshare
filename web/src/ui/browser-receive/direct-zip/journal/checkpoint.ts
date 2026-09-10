import { encodeBase64Url } from '../../../../crypto/bytes'
import type {
  DirectZipCheckpointInputV1, DirectZipCheckpointProposalV1, DirectZipCheckpointV1,
  DirectZipCommitCandidateV1, DirectZipDiscoveryEvidenceV1,
} from '../../../../output/direct-zip/journal'
import { createDirectZipMemberEntryPlanEvidenceV2 } from '../../../../output/direct-zip/journal/records/member-resume'
import type {
  DirectZipEpochCandidateV1, DirectZipWriterCheckpointV1,
} from '../../../../output/direct-zip/writer'
import type { BrowserDirectZipPageAuthority, BrowserDirectZipPages } from './pages'
import { pageState, readPageAuthority } from './pages'
import { digestBytes } from './records'
import type { DirectZipJournalRepository } from '../../../../output/direct-zip/journal'

export async function writerCheckpoint(
  repository: DirectZipJournalRepository,
  checkpoint: DirectZipCheckpointV1 | DirectZipCheckpointProposalV1,
  observationDigest: string,
): Promise<DirectZipWriterCheckpointV1> {
  const authority = await readPageAuthority(repository, checkpoint.operationId, checkpoint)
  const member = checkpoint.currentMember
  const rollback = member === undefined ? undefined :
    await readPageAuthority(repository, checkpoint.operationId, member.rollback)
  return Object.freeze({
    version: 1, operationId: checkpoint.operationId,
    intentDigest: digestBytes(checkpoint.receiveIntentDigest), generation: checkpoint.generation,
    phase: checkpoint.phase, nextEntryOrdinal: checkpoint.entryOrdinal,
    archiveOffset: checkpoint.archiveOffset, committedLength: checkpoint.committedArchiveLength,
    safeResumeBytes: checkpoint.committedSelectedPayloadBytes,
    targetObservationDigest: digestBytes(observationDigest),
    epochRoot: digestBytes(checkpoint.epochRootDigest), pages: pageState(authority),
    ...(member === undefined || rollback === undefined ? {} : {
      member: Object.freeze({
        plan: member.entryPlan,
        source: Object.freeze({
          fileId: member.fileId, revision: member.fileRevision, exactSize: member.exactSize,
          rangeAuthority: member.sourceRangeAuthorityDigest,
        }),
        payloadOffset: member.memberPayloadOffset, crc32Accumulator: member.crc32Accumulator,
        rollback: Object.freeze({
          archiveOffset: member.rollback.archiveOffset,
          safeResumeBytes: member.rollback.safeSelectedPayloadBytes,
          nextEntryOrdinal: member.rollback.entryOrdinal, epochStart: member.rollback.epochStart,
          predecessorEpochRoot: digestBytes(member.rollback.predecessorEpochRootDigest),
          epochContentDigest: digestBytes(member.rollback.epochContentDigest),
          epochRoot: digestBytes(member.rollback.epochRootDigest), pages: pageState(rollback),
        }),
      }),
    }),
    ...(checkpoint.closingReplay === undefined ? {} : {
      closing: Object.freeze({
        centralDirectoryOffset: checkpoint.closingReplay.archiveOffset,
        centralDirectoryBytes: authority.centralBytes, replayStartOrdinal: 0n,
      }),
      ...(checkpoint.closingReplay.completion === undefined ? {} : {
        completion: Object.freeze({
          exactArchiveBytes: checkpoint.closingReplay.completion.exactArchiveBytes,
          predecessorEpochRoot: digestBytes(checkpoint.closingReplay.completion.predecessorEpochRootDigest),
        }),
      }),
    }),
  })
}

export async function writerCandidate(
  repository: DirectZipJournalRepository,
  candidate: DirectZipCommitCandidateV1,
): Promise<DirectZipEpochCandidateV1> {
  const proposed = await writerCheckpoint(repository, candidate.proposedCheckpoint,
    candidate.predecessorTargetObservation.digest)
  return Object.freeze({
    version: 1, kind: candidate.kind, candidateId: candidate.candidateId,
    epochId: candidate.candidateId, operationId: candidate.operationId,
    predecessorGeneration: candidate.predecessorCheckpointGeneration,
    predecessorLength: candidate.predecessorTargetObservation.exactLength,
    predecessorObservationDigest: digestBytes(candidate.predecessorTargetObservation.digest),
    rangeStart: candidate.predecessorTargetObservation.exactLength,
    stagedEnd: proposed.committedLength, contentDigest: digestBytes(candidate.expectedRangeDigest),
    expectedEpochRoot: digestBytes(candidate.proposedCheckpoint.epochRootDigest), proposed,
  })
}

export async function checkpointInput(
  previous: DirectZipCheckpointV1,
  writer: DirectZipWriterCheckpointV1,
  pages: BrowserDirectZipPages,
  discovery: DirectZipDiscoveryEvidenceV1,
  authority: BrowserDirectZipPageAuthority = pages.authority,
): Promise<Omit<DirectZipCheckpointInputV1, 'targetObservation'>> {
  const member = writer.member
  const rollback = member === undefined ? undefined : pages.rollbackAuthority(member.rollback.pages)
  const entryPlan = member === undefined ? undefined : await createDirectZipMemberEntryPlanEvidenceV2(member.plan)
  return {
    operationId: previous.operationId, receiveIntentDigest: previous.receiveIntentDigest,
    targetBindingDigest: previous.targetBindingDigest, policies: previous.policies,
    generation: writer.generation, predecessorCheckpointDigest: previous.digest,
    phase: writer.phase, entryOrdinal: writer.nextEntryOrdinal, discovery,
    archiveOffset: writer.archiveOffset, committedArchiveLength: writer.committedLength,
    committedSelectedPayloadBytes: writer.safeResumeBytes,
    parentBindingDigest: previous.parentBindingDigest, fileBindingDigest: previous.fileBindingDigest,
    epochRootDigest: encodeBase64Url(writer.epochRoot),
    layoutPages: authority.layoutPages, centralPages: authority.centralPages,
    epochPages: authority.epochPages, journalUsage: authority.journalUsage,
    ...(authority.accountingTailPageId === undefined ? {} : { accountingTailPageId: authority.accountingTailPageId }),
    ...(member === undefined || rollback === undefined || entryPlan === undefined ? {} : {
      currentMember: {
        fileId: member.source.fileId, fileRevision: member.source.revision,
        exactSize: member.source.exactSize, sourceRangeAuthorityDigest: member.source.rangeAuthority,
        entryPlan: member.plan, entryPlanCanonicalBytes: entryPlan.canonicalBytes,
        entryPlanDigest: entryPlan.digest, memberPayloadOffset: member.payloadOffset,
        crc32Accumulator: member.crc32Accumulator,
        rollback: {
          archiveOffset: member.rollback.archiveOffset,
          safeSelectedPayloadBytes: member.rollback.safeResumeBytes,
          entryOrdinal: member.rollback.nextEntryOrdinal, epochStart: member.rollback.epochStart,
          predecessorEpochRootDigest: encodeBase64Url(member.rollback.predecessorEpochRoot),
          epochContentDigest: encodeBase64Url(member.rollback.epochContentDigest),
          epochRootDigest: encodeBase64Url(member.rollback.epochRoot),
          layoutPages: rollback.layoutPages, centralPages: rollback.centralPages,
          epochPages: rollback.epochPages, journalUsage: rollback.journalUsage,
          ...(rollback.accountingTailPageId === undefined ? {} : { accountingTailPageId: rollback.accountingTailPageId }),
        },
      },
    }),
    ...(writer.closing === undefined ? {} : {
      closingReplay: {
        archiveOffset: writer.closing.centralDirectoryOffset,
        centralRecordRootDigest: authority.centralPages.rootDigest,
        ...(writer.completion === undefined ? {} : {
          completion: {
            exactArchiveBytes: writer.completion.exactArchiveBytes,
            predecessorEpochRootDigest: encodeBase64Url(writer.completion.predecessorEpochRoot),
          },
        }),
      },
    }),
  }
}
