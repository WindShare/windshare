import { sameRetainedEpochProof } from './retained-epoch'
import { equalCanonicalBytes } from '../../../workspace/canonical'
import type { DirectZipCheckpointProposalV1, DirectZipPolicyDigestsV1, DirectZipStateRowV1 } from '../model'

export function sameJournalPolicies(
  left: DirectZipPolicyDigestsV1,
  right: DirectZipPolicyDigestsV1,
): boolean {
  return left.encodingPolicyDigest === right.encodingPolicyDigest &&
    left.layoutPolicyDigest === right.layoutPolicyDigest &&
    left.checkpointPolicyDigest === right.checkpointPolicyDigest &&
    left.journalBudgetDigest === right.journalBudgetDigest &&
    left.epochPolicyDigest === right.epochPolicyDigest
}

export function sameCheckpointResumeAuthority(
  left: DirectZipStateRowV1['checkpoint'] | DirectZipCheckpointProposalV1,
  right: DirectZipStateRowV1['checkpoint'],
): boolean {
  return left.operationId === right.operationId &&
    left.receiveIntentDigest === right.receiveIntentDigest &&
    left.targetBindingDigest === right.targetBindingDigest &&
    sameJournalPolicies(left.policies, right.policies) &&
    left.phase === right.phase && left.entryOrdinal === right.entryOrdinal &&
    sameCurrentMember(left.currentMember, right.currentMember) &&
    equalCanonicalBytes(left.discovery.cursorCanonicalBytes, right.discovery.cursorCanonicalBytes) &&
    left.discovery.directoryAdmissionDigest === right.discovery.directoryAdmissionDigest &&
    left.discovery.discoveryRootDigest === right.discovery.discoveryRootDigest &&
    left.archiveOffset === right.archiveOffset &&
    left.committedArchiveLength === right.committedArchiveLength &&
    left.committedSelectedPayloadBytes === right.committedSelectedPayloadBytes &&
    left.parentBindingDigest === right.parentBindingDigest &&
    left.fileBindingDigest === right.fileBindingDigest &&
    left.epochRootDigest === right.epochRootDigest &&
    samePageChain(left.layoutPages, right.layoutPages) &&
    samePageChain(left.centralPages, right.centralPages) &&
    samePageChain(left.epochPages, right.epochPages) &&
    sameRetainedEpochProof(left.retainedEpochProof, right.retainedEpochProof) &&
    left.journalUsage.memberCount === right.journalUsage.memberCount &&
    left.journalUsage.canonicalMetadataBytes === right.journalUsage.canonicalMetadataBytes &&
    left.accountingTailPageId === right.accountingTailPageId &&
    sameClosingReplay(left.closingReplay, right.closingReplay)
}

export function sameCurrentMember(
  left: DirectZipStateRowV1['checkpoint']['currentMember'],
  right: DirectZipStateRowV1['checkpoint']['currentMember'],
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.fileId === right.fileId && left.fileRevision === right.fileRevision &&
    left.exactSize === right.exactSize &&
    left.sourceRangeAuthorityDigest === right.sourceRangeAuthorityDigest &&
    left.entryPlan.ordinal === right.entryPlan.ordinal &&
    left.entryPlanDigest === right.entryPlanDigest &&
    equalCanonicalBytes(left.entryPlanCanonicalBytes, right.entryPlanCanonicalBytes) &&
    left.memberPayloadOffset === right.memberPayloadOffset &&
    left.crc32Accumulator === right.crc32Accumulator &&
    sameMemberRollback(left.rollback, right.rollback)
}

export function sameMemberRollback(
  left: NonNullable<DirectZipStateRowV1['checkpoint']['currentMember']>['rollback'],
  right: NonNullable<DirectZipStateRowV1['checkpoint']['currentMember']>['rollback'],
): boolean {
  return left.archiveOffset === right.archiveOffset &&
    left.safeSelectedPayloadBytes === right.safeSelectedPayloadBytes &&
    left.entryOrdinal === right.entryOrdinal && left.epochStart === right.epochStart &&
    left.predecessorEpochRootDigest === right.predecessorEpochRootDigest &&
    left.epochContentDigest === right.epochContentDigest &&
    left.epochRootDigest === right.epochRootDigest &&
    samePageChain(left.layoutPages, right.layoutPages) &&
    samePageChain(left.centralPages, right.centralPages) &&
    samePageChain(left.epochPages, right.epochPages) &&
    sameRetainedEpochProof(left.retainedEpochProof, right.retainedEpochProof) &&
    left.journalUsage.memberCount === right.journalUsage.memberCount &&
    left.journalUsage.canonicalMetadataBytes === right.journalUsage.canonicalMetadataBytes &&
    left.accountingTailPageId === right.accountingTailPageId
}

export function samePageChain(
  left: DirectZipStateRowV1['checkpoint']['layoutPages'],
  right: DirectZipStateRowV1['checkpoint']['layoutPages'],
): boolean {
  return left.chainId === right.chainId && left.rootDigest === right.rootDigest &&
    left.pageCount === right.pageCount && left.recordCount === right.recordCount &&
    left.canonicalMetadataBytes === right.canonicalMetadataBytes
}

export function sameClosingReplay(
  left: DirectZipStateRowV1['checkpoint']['closingReplay'],
  right: DirectZipStateRowV1['checkpoint']['closingReplay'],
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.archiveOffset === right.archiveOffset &&
    left.centralRecordRootDigest === right.centralRecordRootDigest
}
