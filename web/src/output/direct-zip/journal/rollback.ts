import { canonicalDigest, canonicalFrame, canonicalRecord, canonicalU64, equalCanonicalBytes, snapshotIdentity } from '../../workspace/canonical'
import type { DirectZipCheckpointV1, DirectZipRollbackCandidateV1, DirectZipPendingCandidateV1 } from './model'
import { DIRECT_ZIP_CANDIDATE_ROLLBACK, DIRECT_ZIP_JOURNAL_SCHEMA_VERSION } from './model'
import { validateDirectZipCheckpointProposalV1, validateDirectZipCommitCandidateV1 } from './records'
import { assertCanonicalProjection, digestFrame, directZipCandidateId, identityFrame, requireJournalVersion, requirePositiveU64, snapshotDigest } from './records/canonical-fields'
import { validateDirectZipTargetObservationV1 } from './records/target-observation'
import { sameJournalPolicies, samePageChain } from './records/checkpoint-authority'

import { sameRetainedEpochProof } from './records/retained-epoch'

const ROLLBACK_CANDIDATE_DOMAIN = 'windshare/direct-zip-rollback-candidate/v1'

export type DirectZipRollbackCandidateInputV1 = Omit<
  DirectZipRollbackCandidateV1,
  'id' | 'schemaVersion' | 'kind' | 'kindByte' | 'canonicalBytes' | 'digest'
>

export async function createDirectZipRollbackCandidateV1(
  input: DirectZipRollbackCandidateInputV1,
): Promise<DirectZipRollbackCandidateV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const candidateId = snapshotIdentity(input.candidateId, 16, 'rollback candidate ID')
  const leaseId = snapshotIdentity(input.leaseId, 16, 'lease ID')
  const predecessorCheckpointGeneration = requirePositiveU64(
    input.predecessorCheckpointGeneration, 'predecessor checkpoint generation',
  )
  const predecessorCheckpointDigest = snapshotDigest(
    input.predecessorCheckpointDigest, 'predecessor checkpoint digest',
  )
  const predecessorTargetObservation = await validateDirectZipTargetObservationV1(
    input.predecessorTargetObservation,
  )
  const proposedCheckpoint = await validateDirectZipCheckpointProposalV1(input.proposedCheckpoint)
  if (predecessorTargetObservation.operationId !== operationId ||
      proposedCheckpoint.operationId !== operationId ||
      proposedCheckpoint.generation !== predecessorCheckpointGeneration + 1n ||
      proposedCheckpoint.predecessorCheckpointDigest !== predecessorCheckpointDigest ||
      proposedCheckpoint.phase !== 'between-members' ||
      proposedCheckpoint.parentBindingDigest !== predecessorTargetObservation.parentBindingDigest ||
      proposedCheckpoint.fileBindingDigest !== predecessorTargetObservation.fileBindingDigest ||
      proposedCheckpoint.committedArchiveLength >= predecessorTargetObservation.exactLength) {
    throw new TypeError('Direct ZIP rollback must restore a shorter completed-member boundary')
  }
  const canonicalBytes = canonicalRecord(ROLLBACK_CANDIDATE_DOMAIN, 1, [
    identityFrame(operationId, 16, 'operation ID'),
    identityFrame(candidateId, 16, 'rollback candidate ID'),
    identityFrame(leaseId, 16, 'lease ID'),
    canonicalFrame(canonicalU64(predecessorCheckpointGeneration)),
    digestFrame(predecessorCheckpointDigest, 'predecessor checkpoint digest'),
    canonicalFrame(predecessorTargetObservation.canonicalBytes),
    canonicalFrame(proposedCheckpoint.canonicalBytes),
  ])
  return Object.freeze({
    id: directZipCandidateId(operationId, candidateId),
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    kind: 'rollback',
    kindByte: DIRECT_ZIP_CANDIDATE_ROLLBACK,
    operationId, candidateId, leaseId, predecessorCheckpointGeneration,
    predecessorCheckpointDigest, predecessorTargetObservation, proposedCheckpoint,
    canonicalBytes, digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateDirectZipRollbackCandidateV1(
  input: DirectZipRollbackCandidateV1,
): Promise<DirectZipRollbackCandidateV1> {
  requireJournalVersion(input.schemaVersion)
  const rebuilt = await createDirectZipRollbackCandidateV1(input)
  if (input.id !== rebuilt.id || input.kind !== rebuilt.kind || input.kindByte !== rebuilt.kindByte) {
    throw new TypeError('Direct ZIP rollback candidate projections disagree')
  }
  assertCanonicalProjection(input, rebuilt, 'Direct ZIP rollback candidate')
  return rebuilt
}

export function validateDirectZipPendingCandidateV1(
  input: DirectZipPendingCandidateV1,
): Promise<DirectZipPendingCandidateV1> {
  return input.kind === 'rollback'
    ? validateDirectZipRollbackCandidateV1(input)
    : validateDirectZipCommitCandidateV1(input)
}

/** Destructive authority is derived only from the active member's previously committed boundary. */
export function assertDirectZipRollbackPredecessorV1(
  checkpoint: DirectZipCheckpointV1,
  candidate: DirectZipRollbackCandidateV1,
): void {
  const rollback = checkpoint.currentMember?.rollback
  if (rollback === undefined) throw new TypeError('Direct ZIP rollback requires an active member')
  const proposed = candidate.proposedCheckpoint
  const retainedEpochProof = rollback.epochStart === rollback.archiveOffset
    ? rollback.retainedEpochProof
    : Object.freeze({
        start: rollback.epochStart, end: rollback.archiveOffset,
        predecessorRootDigest: rollback.predecessorEpochRootDigest,
        contentDigest: rollback.epochContentDigest, epochRootDigest: rollback.epochRootDigest,
      })
  if (candidate.predecessorCheckpointDigest !== checkpoint.digest ||
      candidate.predecessorCheckpointGeneration !== checkpoint.generation ||
      candidate.predecessorTargetObservation.digest !== checkpoint.targetObservation.digest ||
      proposed.receiveIntentDigest !== checkpoint.receiveIntentDigest ||
      proposed.targetBindingDigest !== checkpoint.targetBindingDigest ||
      !sameJournalPolicies(proposed.policies, checkpoint.policies) ||
      !equalCanonicalBytes(proposed.discovery.cursorCanonicalBytes, checkpoint.discovery.cursorCanonicalBytes) ||
      proposed.discovery.directoryAdmissionDigest !== checkpoint.discovery.directoryAdmissionDigest ||
      proposed.discovery.discoveryRootDigest !== checkpoint.discovery.discoveryRootDigest ||
      proposed.entryOrdinal !== rollback.entryOrdinal || proposed.archiveOffset !== rollback.archiveOffset ||
      proposed.committedSelectedPayloadBytes !== rollback.safeSelectedPayloadBytes ||
      proposed.epochRootDigest !== rollback.epochRootDigest ||
      !samePageChain(proposed.layoutPages, rollback.layoutPages) ||
      !samePageChain(proposed.centralPages, rollback.centralPages) ||
      !samePageChain(proposed.epochPages, rollback.epochPages) ||
      !sameRetainedEpochProof(proposed.retainedEpochProof, retainedEpochProof) ||
      proposed.journalUsage.memberCount !== rollback.journalUsage.memberCount ||
      proposed.journalUsage.canonicalMetadataBytes !== rollback.journalUsage.canonicalMetadataBytes ||
      proposed.accountingTailPageId !== rollback.accountingTailPageId) {
    throw new TypeError('Direct ZIP rollback escaped its committed active-member boundary')
  }
}
