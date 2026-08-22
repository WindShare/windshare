import { encodeBase64Url } from '../../../crypto/bytes'
import type { ArtifactChoiceID } from '../../../transfer/intent'
import { requireDirectZipFsaOffset } from '../format'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalText,
  canonicalU8,
  canonicalU64,
  snapshotCanonicalBytes,
  snapshotIdentity,
} from '../../workspace/canonical'
import {
  admitDirectZipJournalPageV1,
  validateDirectZipJournalBudgetUsageV1,
} from './budget'
import { validateDirectZipRecoveryGateV1 } from './recovery-gate'
import {
  DIRECT_ZIP_CANDIDATE_BOOTSTRAP,
  DIRECT_ZIP_CANDIDATE_CLOSING,
  DIRECT_ZIP_CANDIDATE_EPOCH,
  DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
  type DirectZipBootstrapCandidateV1,
  type DirectZipCheckpointProposalV1,
  type DirectZipCheckpointV1,
  type DirectZipCommitCandidateV1,
  type DirectZipImmutablePageV1,
  type DirectZipJournalBudgetUsageV1,
  type DirectZipPageAccountingPredecessorV1,
  type DirectZipPageKind,
  type DirectZipStateRowV1,
} from './model'
import {
  assertCanonicalProjection,
  canonicalAccountingPredecessor,
  canonicalBudgetUsage,
  canonicalClosingReplay,
  canonicalDiscoveryEvidence,
  canonicalPageChain,
  canonicalRanking,
  checkedBudgetSubtraction,
  checkedU64Addition,
  checkpointPhaseByte,
  digestFrame,
  directZipCandidateDomain,
  directZipCandidateId,
  directZipPageDomain,
  directZipPageId,
  directZipPageKindByte,
  directZipStateId,
  fixedFrame,
  identityFrame,
  invalidCandidateKind,
  optionalCanonicalText,
  optionalDigest,
  optionalDigestFrame,
  optionalTextFrame,
  policyFrames,
  requireCheckpointPhase,
  requireJournalVersion,
  requirePageKind,
  requirePageOrdinal,
  requirePositiveU64,
  requireU64,
  snapshotAccountingPredecessor,
  snapshotCanonicalText,
  snapshotClosingReplay,
  snapshotDigest,
  snapshotDiscoveryEvidence,
  snapshotFixedBase64,
  snapshotPageChain,
  snapshotPageEntries,
  snapshotPolicies,
  snapshotRanking,
} from './records/canonical-fields'
import {
  canonicalCurrentMember as canonicalMemberResume,
  snapshotCurrentMember,
  validateCheckpointRecoveryAuthority,
} from './records/member-resume'
import {
  createDirectZipTargetObservationV1,
  validateDirectZipTargetObservationV1,
} from './records/target-observation'

const DIRECT_ZIP_CHECKPOINT_DOMAIN = 'windshare/direct-zip-checkpoint/v1'
const DIRECT_ZIP_CHECKPOINT_PROPOSAL_DOMAIN = 'windshare/direct-zip-checkpoint-proposal/v1'
const DIRECT_ZIP_BOOTSTRAP_CANDIDATE_DOMAIN = 'windshare/direct-zip-bootstrap-candidate/v1'
const DIRECT_ZIP_PAGE_ROOT_DOMAIN = 'windshare/direct-zip-page-root/v1'
const ZERO_DIGEST = encodeBase64Url(new Uint8Array(32))
const PROPOSAL_VALIDATION_MARKER_DIGEST = encodeBase64Url(new Uint8Array(32).fill(0xff))

export type DirectZipCheckpointInputV1 = Omit<
  DirectZipCheckpointV1,
  'schemaVersion' | 'canonicalBytes' | 'digest'
>

export async function createDirectZipCheckpointV1(
  input: DirectZipCheckpointInputV1,
): Promise<DirectZipCheckpointV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const receiveIntentDigest = snapshotDigest(input.receiveIntentDigest, 'receive intent digest')
  const targetBindingDigest = snapshotDigest(input.targetBindingDigest, 'target binding digest')
  const policies = await snapshotPolicies(input.policies)
  const generation = requirePositiveU64(input.generation, 'checkpoint generation')
  const predecessorCheckpointDigest = optionalDigest(
    input.predecessorCheckpointDigest,
    'predecessor checkpoint digest',
  )
  const candidateLineageDigest = optionalDigest(
    input.candidateLineageDigest,
    'candidate lineage digest',
  )
  const phase = requireCheckpointPhase(input.phase)
  const entryOrdinal = requireU64(input.entryOrdinal, 'entry ordinal')
  const currentMember = await snapshotCurrentMember(input.currentMember, phase)
  const discovery = snapshotDiscoveryEvidence(input.discovery)
  requireDirectZipFsaOffset(input.archiveOffset, 'checkpoint archive offset')
  requireDirectZipFsaOffset(input.committedArchiveLength, 'committed archive length')
  const committedSelectedPayloadBytes = requireU64(
    input.committedSelectedPayloadBytes,
    'committed selected payload bytes',
  )
  if (input.archiveOffset !== input.committedArchiveLength) {
    throw new TypeError('checkpoint archive offset must equal its committed target length')
  }
  const parentBindingDigest = snapshotDigest(input.parentBindingDigest, 'parent binding digest')
  const fileBindingDigest = snapshotDigest(input.fileBindingDigest, 'file binding digest')
  const targetObservation = await validateDirectZipTargetObservationV1(input.targetObservation)
  if (targetObservation.operationId !== operationId ||
      targetObservation.parentBindingDigest !== parentBindingDigest ||
      targetObservation.fileBindingDigest !== fileBindingDigest ||
      targetObservation.exactLength !== input.committedArchiveLength) {
    throw new TypeError('checkpoint target observation disagrees with committed authority')
  }
  const epochRootDigest = snapshotFixedBase64(
    input.epochRootDigest,
    32,
    'epoch root digest',
    true,
  )
  if (epochRootDigest !== targetObservation.epochRootDigest) {
    throw new TypeError('checkpoint epoch root disagrees with its target observation')
  }
  const layoutPages = snapshotPageChain(input.layoutPages, 'layout')
  const centralPages = snapshotPageChain(input.centralPages, 'central')
  const epochPages = snapshotPageChain(input.epochPages, 'epoch')
  const expectedLayoutRecords = phase === 'inside-member' ? entryOrdinal + 1n : entryOrdinal
  if (layoutPages.recordCount !== expectedLayoutRecords ||
      centralPages.recordCount !== entryOrdinal) {
    throw new TypeError('checkpoint page records disagree with its member phase')
  }
  const journalUsage = validateDirectZipJournalBudgetUsageV1(input.journalUsage)
  const accountedMetadata = layoutPages.canonicalMetadataBytes +
    centralPages.canonicalMetadataBytes + epochPages.canonicalMetadataBytes
  if (journalUsage.memberCount !== layoutPages.recordCount ||
      journalUsage.canonicalMetadataBytes !== accountedMetadata) {
    throw new TypeError('checkpoint journal usage disagrees with its immutable page roots')
  }
  const accountingTailPageId = optionalCanonicalText(
    input.accountingTailPageId,
    'accounting tail page ID',
  )
  if (accountingTailPageId === undefined &&
      (journalUsage.memberCount !== 0n || journalUsage.canonicalMetadataBytes !== 0n)) {
    throw new TypeError('non-empty journal accounting requires a tail page')
  }
  const closingReplay = snapshotClosingReplay(input.closingReplay, phase)
  validateCheckpointRecoveryAuthority({
    currentMember,
    entryOrdinal,
    archiveOffset: input.archiveOffset,
    committedSelectedPayloadBytes,
    layoutPages,
    centralPages,
    epochPages,
    closingReplay,
    committedArchiveLength: input.committedArchiveLength,
  })
  const canonicalBytes = canonicalRecord(DIRECT_ZIP_CHECKPOINT_DOMAIN, 1, [
    identityFrame(operationId, 16, 'operation ID'),
    digestFrame(receiveIntentDigest, 'receive intent digest'),
    digestFrame(targetBindingDigest, 'target binding digest'),
    ...policyFrames(policies),
    canonicalFrame(canonicalU64(generation)),
    optionalDigestFrame(predecessorCheckpointDigest, 'predecessor checkpoint digest'),
    optionalDigestFrame(candidateLineageDigest, 'candidate lineage digest'),
    canonicalFrame(canonicalU8(checkpointPhaseByte(phase))),
    canonicalFrame(canonicalU64(entryOrdinal)),
    canonicalFrame(canonicalMemberResume(currentMember)),
    canonicalFrame(canonicalDiscoveryEvidence(discovery)),
    canonicalFrame(canonicalU64(input.archiveOffset)),
    canonicalFrame(canonicalU64(input.committedArchiveLength)),
    canonicalFrame(canonicalU64(committedSelectedPayloadBytes)),
    digestFrame(parentBindingDigest, 'parent binding digest'),
    digestFrame(fileBindingDigest, 'file binding digest'),
    canonicalFrame(targetObservation.canonicalBytes),
    fixedFrame(epochRootDigest, 32, 'epoch root digest', true),
    canonicalFrame(canonicalPageChain(layoutPages)),
    canonicalFrame(canonicalPageChain(centralPages)),
    canonicalFrame(canonicalPageChain(epochPages)),
    canonicalFrame(canonicalBudgetUsage(journalUsage)),
    optionalTextFrame(accountingTailPageId),
    canonicalFrame(canonicalClosingReplay(closingReplay)),
  ])
  return Object.freeze({
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    operationId,
    receiveIntentDigest,
    targetBindingDigest,
    policies,
    generation,
    ...(predecessorCheckpointDigest === undefined ? {} : { predecessorCheckpointDigest }),
    ...(candidateLineageDigest === undefined ? {} : { candidateLineageDigest }),
    phase,
    entryOrdinal,
    ...(currentMember === undefined ? {} : { currentMember }),
    discovery,
    archiveOffset: input.archiveOffset,
    committedArchiveLength: input.committedArchiveLength,
    committedSelectedPayloadBytes,
    parentBindingDigest,
    fileBindingDigest,
    targetObservation,
    epochRootDigest,
    layoutPages,
    centralPages,
    epochPages,
    journalUsage,
    ...(accountingTailPageId === undefined ? {} : { accountingTailPageId }),
    ...(closingReplay === undefined ? {} : { closingReplay }),
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateDirectZipCheckpointV1(
  input: DirectZipCheckpointV1,
): Promise<DirectZipCheckpointV1> {
  requireJournalVersion(input.schemaVersion)
  const rebuilt = await createDirectZipCheckpointV1(input)
  assertCanonicalProjection(input, rebuilt, 'Direct ZIP checkpoint')
  return rebuilt
}

export type DirectZipCheckpointProposalInputV1 = Omit<
  DirectZipCheckpointInputV1,
  'candidateLineageDigest' | 'targetObservation'
>

export async function createDirectZipCheckpointProposalV1(
  input: DirectZipCheckpointProposalInputV1,
): Promise<DirectZipCheckpointProposalV1> {
  if (input.closingReplay?.completion !== undefined) {
    throw new TypeError('Direct ZIP proposal cannot claim a post-close completion')
  }
  const expectedObservation = await createDirectZipTargetObservationV1({
    operationId: input.operationId,
    parentBindingDigest: input.parentBindingDigest,
    fileBindingDigest: input.fileBindingDigest,
    // This observation is validation-only and is excluded from the proposal's canonical record.
    ownershipMarkerDigest: PROPOSAL_VALIDATION_MARKER_DIGEST,
    exactLength: input.committedArchiveLength,
    lastModifiedMilliseconds: 0,
    epochRootDigest: input.epochRootDigest,
  })
  const checked = await createDirectZipCheckpointV1({
    ...input,
    targetObservation: expectedObservation,
  })
  const canonicalBytes = canonicalRecord(DIRECT_ZIP_CHECKPOINT_PROPOSAL_DOMAIN, 1, [
    identityFrame(checked.operationId, 16, 'operation ID'),
    digestFrame(checked.receiveIntentDigest, 'receive intent digest'),
    digestFrame(checked.targetBindingDigest, 'target binding digest'),
    ...policyFrames(checked.policies),
    canonicalFrame(canonicalU64(checked.generation)),
    optionalDigestFrame(checked.predecessorCheckpointDigest, 'predecessor checkpoint digest'),
    canonicalFrame(canonicalU8(checkpointPhaseByte(checked.phase))),
    canonicalFrame(canonicalU64(checked.entryOrdinal)),
    canonicalFrame(canonicalMemberResume(checked.currentMember)),
    canonicalFrame(canonicalDiscoveryEvidence(checked.discovery)),
    canonicalFrame(canonicalU64(checked.archiveOffset)),
    canonicalFrame(canonicalU64(checked.committedArchiveLength)),
    canonicalFrame(canonicalU64(checked.committedSelectedPayloadBytes)),
    digestFrame(checked.parentBindingDigest, 'parent binding digest'),
    digestFrame(checked.fileBindingDigest, 'file binding digest'),
    fixedFrame(checked.epochRootDigest, 32, 'epoch root digest', true),
    canonicalFrame(canonicalPageChain(checked.layoutPages)),
    canonicalFrame(canonicalPageChain(checked.centralPages)),
    canonicalFrame(canonicalPageChain(checked.epochPages)),
    canonicalFrame(canonicalBudgetUsage(checked.journalUsage)),
    optionalTextFrame(checked.accountingTailPageId),
    canonicalFrame(canonicalClosingReplay(checked.closingReplay)),
  ])
  return Object.freeze({
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    operationId: checked.operationId,
    receiveIntentDigest: checked.receiveIntentDigest,
    targetBindingDigest: checked.targetBindingDigest,
    policies: checked.policies,
    generation: checked.generation,
    ...(checked.predecessorCheckpointDigest === undefined ? {} : {
      predecessorCheckpointDigest: checked.predecessorCheckpointDigest,
    }),
    phase: checked.phase,
    entryOrdinal: checked.entryOrdinal,
    ...(checked.currentMember === undefined ? {} : { currentMember: checked.currentMember }),
    discovery: checked.discovery,
    archiveOffset: checked.archiveOffset,
    committedArchiveLength: checked.committedArchiveLength,
    committedSelectedPayloadBytes: checked.committedSelectedPayloadBytes,
    parentBindingDigest: checked.parentBindingDigest,
    fileBindingDigest: checked.fileBindingDigest,
    epochRootDigest: checked.epochRootDigest,
    layoutPages: checked.layoutPages,
    centralPages: checked.centralPages,
    epochPages: checked.epochPages,
    journalUsage: checked.journalUsage,
    ...(checked.accountingTailPageId === undefined ? {} : {
      accountingTailPageId: checked.accountingTailPageId,
    }),
    ...(checked.closingReplay === undefined ? {} : { closingReplay: checked.closingReplay }),
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateDirectZipCheckpointProposalV1(
  input: DirectZipCheckpointProposalV1,
): Promise<DirectZipCheckpointProposalV1> {
  requireJournalVersion(input.schemaVersion)
  const rebuilt = await createDirectZipCheckpointProposalV1(input)
  assertCanonicalProjection(input, rebuilt, 'Direct ZIP checkpoint proposal')
  return rebuilt
}

export interface DirectZipImmutablePageInputV1 {
  readonly operationId: string
  readonly pageKind: DirectZipPageKind
  readonly chainId: string
  readonly pageOrdinal: number
  readonly predecessorRootDigest: string
  readonly canonicalEntries: readonly Uint8Array[]
  readonly accountingPredecessor: DirectZipPageAccountingPredecessorV1
  readonly previousBudgetUsage: DirectZipJournalBudgetUsageV1
  readonly previousChainRecordCount: bigint
  readonly previousChainCanonicalMetadataBytes: bigint
}

export async function createDirectZipImmutablePageV1(
  input: DirectZipImmutablePageInputV1,
): Promise<DirectZipImmutablePageV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const pageKind = requirePageKind(input.pageKind)
  const pageKindByte = directZipPageKindByte(pageKind)
  const chainId = snapshotIdentity(input.chainId, 16, 'page chain ID')
  const pageOrdinal = requirePageOrdinal(input.pageOrdinal)
  const predecessorRootDigest = snapshotFixedBase64(
    input.predecessorRootDigest,
    32,
    'page predecessor root',
    true,
  )
  if ((pageOrdinal === 0) !== (predecessorRootDigest === ZERO_DIGEST)) {
    throw new TypeError('page ordinal and predecessor root do not start the same chain')
  }
  if (pageOrdinal === 0 &&
      (input.previousChainRecordCount !== 0n || input.previousChainCanonicalMetadataBytes !== 0n)) {
    throw new TypeError('first page must start with empty chain accounting')
  }
  const canonicalEntries = snapshotPageEntries(input.canonicalEntries)
  const memberCountDelta = pageKind === 'layout' ? BigInt(canonicalEntries.length) : 0n
  const accountingPredecessor = snapshotAccountingPredecessor(input.accountingPredecessor)
  const canonicalBytes = canonicalRecord(directZipPageDomain(pageKind), 1, [
    identityFrame(operationId, 16, 'operation ID'),
    identityFrame(chainId, 16, 'page chain ID'),
    canonicalFrame(canonicalU64(BigInt(pageOrdinal))),
    fixedFrame(predecessorRootDigest, 32, 'page predecessor root', true),
    canonicalFrame(canonicalAccountingPredecessor(accountingPredecessor)),
    canonicalFrame(canonicalU64(BigInt(canonicalEntries.length))),
    ...canonicalEntries.map(canonicalFrame),
  ])
  const budgetUsage = admitDirectZipJournalPageV1({
    usage: input.previousBudgetUsage,
    pageCanonicalBytes: canonicalBytes.byteLength,
    pageRecordCount: canonicalEntries.length,
    memberCountDelta,
  })
  const chainRecordCount = checkedU64Addition(
    requireU64(input.previousChainRecordCount, 'previous chain record count'),
    BigInt(canonicalEntries.length),
    'page chain record count',
  )
  const chainCanonicalMetadataBytes = checkedU64Addition(
    requireU64(input.previousChainCanonicalMetadataBytes, 'previous chain metadata bytes'),
    BigInt(canonicalBytes.byteLength),
    'page chain metadata bytes',
  )
  const digest = await canonicalDigest(canonicalBytes)
  const chainRootDigest = await canonicalDigest(canonicalRecord(DIRECT_ZIP_PAGE_ROOT_DOMAIN, 1, [
    canonicalFrame(canonicalU8(pageKindByte)),
    canonicalFrame(canonicalU64(BigInt(pageOrdinal))),
    fixedFrame(predecessorRootDigest, 32, 'page predecessor root', true),
    digestFrame(digest, 'page digest'),
  ]))
  return Object.freeze({
    id: directZipPageId(operationId, pageKind, chainId, pageOrdinal),
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    operationId,
    pageKind,
    pageKindByte,
    chainId,
    pageOrdinal,
    predecessorRootDigest,
    canonicalEntries,
    entryCount: canonicalEntries.length,
    memberCountDelta,
    chainRecordCount,
    chainCanonicalMetadataBytes,
    accountingPredecessor,
    budgetUsage,
    canonicalBytes,
    digest,
    chainRootDigest,
  })
}

export async function validateDirectZipImmutablePageV1(
  input: DirectZipImmutablePageV1,
): Promise<DirectZipImmutablePageV1> {
  requireJournalVersion(input.schemaVersion)
  const previousBudgetUsage = Object.freeze({
    memberCount: checkedBudgetSubtraction(
      input.budgetUsage.memberCount,
      input.memberCountDelta,
      'page member accounting',
    ),
    canonicalMetadataBytes: checkedBudgetSubtraction(
      input.budgetUsage.canonicalMetadataBytes,
      BigInt(input.canonicalBytes.byteLength),
      'page metadata accounting',
    ),
  })
  const rebuilt = await createDirectZipImmutablePageV1({
    ...input,
    previousBudgetUsage,
    previousChainRecordCount: checkedBudgetSubtraction(
      input.chainRecordCount,
      BigInt(input.entryCount),
      'page chain record accounting',
    ),
    previousChainCanonicalMetadataBytes: checkedBudgetSubtraction(
      input.chainCanonicalMetadataBytes,
      BigInt(input.canonicalBytes.byteLength),
      'page chain metadata accounting',
    ),
  })
  if (input.id !== rebuilt.id || input.pageKindByte !== rebuilt.pageKindByte ||
      input.entryCount !== rebuilt.entryCount || input.chainRootDigest !== rebuilt.chainRootDigest) {
    throw new TypeError('Direct ZIP immutable page projections disagree')
  }
  assertCanonicalProjection(input, rebuilt, 'Direct ZIP immutable page')
  return rebuilt
}

export type DirectZipBootstrapCandidateInputV1 = Omit<
  DirectZipBootstrapCandidateV1,
  'id' | 'schemaVersion' | 'kind' | 'kindByte' | 'canonicalBytes' | 'digest'
>

export async function createDirectZipBootstrapCandidateV1(
  input: DirectZipBootstrapCandidateInputV1,
): Promise<DirectZipBootstrapCandidateV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const candidateId = snapshotIdentity(input.candidateId, 16, 'bootstrap candidate ID')
  const leaseId = snapshotIdentity(input.leaseId, 16, 'lease ID')
  const leaseGeneration = requirePositiveU64(input.leaseGeneration, 'lease generation')
  const selectionCanonicalBytes = snapshotCanonicalBytes(input.selectionCanonicalBytes)
  const artifactCanonicalBytes = snapshotCanonicalBytes(input.artifactCanonicalBytes)
  const choiceIdentityCanonicalBytes = snapshotCanonicalBytes(input.choiceIdentityCanonicalBytes)
  const choiceId = snapshotIdentity(input.choiceId, 32, 'artifact choice ID') as ArtifactChoiceID
  const preClickRanking = snapshotRanking(input.preClickRanking, choiceId)
  const stablePhysicalName = snapshotCanonicalText(
    input.stablePhysicalName,
    'stable physical name',
  )
  const ownershipNonce = snapshotIdentity(input.ownershipNonce, 32, 'ownership nonce')
  const targetBindingDigest = snapshotDigest(input.targetBindingDigest, 'target binding digest')
  const policies = await snapshotPolicies(input.policies)
  const parentHandleId = snapshotCanonicalText(input.parentHandleId, 'parent handle ID')
  const canonicalBytes = canonicalRecord(DIRECT_ZIP_BOOTSTRAP_CANDIDATE_DOMAIN, 1, [
    identityFrame(operationId, 16, 'operation ID'),
    identityFrame(candidateId, 16, 'bootstrap candidate ID'),
    identityFrame(leaseId, 16, 'lease ID'),
    canonicalFrame(canonicalU64(leaseGeneration)),
    canonicalFrame(selectionCanonicalBytes),
    canonicalFrame(artifactCanonicalBytes),
    canonicalFrame(choiceIdentityCanonicalBytes),
    identityFrame(choiceId, 32, 'artifact choice ID'),
    canonicalFrame(canonicalRanking(preClickRanking)),
    canonicalFrame(canonicalText(stablePhysicalName)),
    identityFrame(ownershipNonce, 32, 'ownership nonce'),
    digestFrame(targetBindingDigest, 'target binding digest'),
    ...policyFrames(policies),
    canonicalFrame(canonicalText(parentHandleId)),
  ])
  return Object.freeze({
    id: directZipCandidateId(operationId, candidateId),
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    kind: 'bootstrap',
    kindByte: DIRECT_ZIP_CANDIDATE_BOOTSTRAP,
    operationId,
    candidateId,
    leaseId,
    leaseGeneration,
    selectionCanonicalBytes,
    artifactCanonicalBytes,
    choiceIdentityCanonicalBytes,
    choiceId,
    preClickRanking,
    stablePhysicalName,
    ownershipNonce,
    targetBindingDigest,
    policies,
    parentHandleId,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateDirectZipBootstrapCandidateV1(
  input: DirectZipBootstrapCandidateV1,
): Promise<DirectZipBootstrapCandidateV1> {
  requireJournalVersion(input.schemaVersion)
  if (input.kind !== 'bootstrap' || input.kindByte !== DIRECT_ZIP_CANDIDATE_BOOTSTRAP) {
    throw new TypeError('Direct ZIP bootstrap candidate kind is invalid')
  }
  const rebuilt = await createDirectZipBootstrapCandidateV1(input)
  if (input.id !== rebuilt.id) throw new TypeError('Direct ZIP bootstrap candidate ID is invalid')
  assertCanonicalProjection(input, rebuilt, 'Direct ZIP bootstrap candidate')
  return rebuilt
}

export type DirectZipCommitCandidateInputV1 = Omit<
  DirectZipCommitCandidateV1,
  'id' | 'schemaVersion' | 'kindByte' | 'canonicalBytes' | 'digest'
>

export async function createDirectZipCommitCandidateV1(
  input: DirectZipCommitCandidateInputV1,
): Promise<DirectZipCommitCandidateV1> {
  const kind = input.kind === 'epoch' || input.kind === 'closing'
    ? input.kind
    : invalidCandidateKind()
  const kindByte = kind === 'epoch' ? DIRECT_ZIP_CANDIDATE_EPOCH : DIRECT_ZIP_CANDIDATE_CLOSING
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const candidateId = snapshotIdentity(input.candidateId, 16, 'candidate ID')
  const leaseId = snapshotIdentity(input.leaseId, 16, 'lease ID')
  const predecessorCheckpointGeneration = requirePositiveU64(
    input.predecessorCheckpointGeneration,
    'predecessor checkpoint generation',
  )
  const predecessorCheckpointDigest = snapshotDigest(
    input.predecessorCheckpointDigest,
    'predecessor checkpoint digest',
  )
  const expectedRangeDigest = snapshotDigest(input.expectedRangeDigest, 'expected range digest')
  const predecessorTargetObservation = await validateDirectZipTargetObservationV1(
    input.predecessorTargetObservation,
  )
  const proposedCheckpoint = await validateDirectZipCheckpointProposalV1(input.proposedCheckpoint)
  if (predecessorTargetObservation.operationId !== operationId ||
      proposedCheckpoint.operationId !== operationId ||
      proposedCheckpoint.generation !== predecessorCheckpointGeneration + 1n ||
      proposedCheckpoint.predecessorCheckpointDigest !== predecessorCheckpointDigest ||
      proposedCheckpoint.closingReplay?.completion !== undefined ||
      (kind === 'closing') !== (proposedCheckpoint.phase === 'closing')) {
    throw new TypeError('Direct ZIP candidate does not advance its exact checkpoint predecessor')
  }
  const canonicalBytes = canonicalRecord(directZipCandidateDomain(kind), 1, [
    identityFrame(operationId, 16, 'operation ID'),
    identityFrame(candidateId, 16, 'candidate ID'),
    identityFrame(leaseId, 16, 'lease ID'),
    canonicalFrame(canonicalU64(predecessorCheckpointGeneration)),
    digestFrame(predecessorCheckpointDigest, 'predecessor checkpoint digest'),
    digestFrame(expectedRangeDigest, 'expected range digest'),
    canonicalFrame(predecessorTargetObservation.canonicalBytes),
    canonicalFrame(proposedCheckpoint.canonicalBytes),
  ])
  return Object.freeze({
    id: directZipCandidateId(operationId, candidateId),
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    kind,
    kindByte,
    operationId,
    candidateId,
    leaseId,
    predecessorCheckpointGeneration,
    predecessorCheckpointDigest,
    expectedRangeDigest,
    predecessorTargetObservation,
    proposedCheckpoint,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateDirectZipCommitCandidateV1(
  input: DirectZipCommitCandidateV1,
): Promise<DirectZipCommitCandidateV1> {
  requireJournalVersion(input.schemaVersion)
  const rebuilt = await createDirectZipCommitCandidateV1(input)
  if (input.id !== rebuilt.id || input.kindByte !== rebuilt.kindByte) {
    throw new TypeError('Direct ZIP commit candidate projections disagree')
  }
  assertCanonicalProjection(input, rebuilt, 'Direct ZIP commit candidate')
  return rebuilt
}

export async function createDirectZipStateRowV1(
  checkpointInput: DirectZipCheckpointV1,
  leaseIdInput: string,
  recoveryGateInput?: DirectZipStateRowV1['recoveryGate'],
): Promise<DirectZipStateRowV1> {
  const checkpoint = await validateDirectZipCheckpointV1(checkpointInput)
  const leaseId = snapshotIdentity(leaseIdInput, 16, 'lease ID')
  const recoveryGate = recoveryGateInput === undefined
    ? undefined
    : await validateDirectZipRecoveryGateV1(recoveryGateInput)
  if (recoveryGate !== undefined &&
      (recoveryGate.operationId !== checkpoint.operationId ||
       recoveryGate.receiveIntentDigest !== checkpoint.receiveIntentDigest ||
       recoveryGate.checkpointDigest !== checkpoint.digest)) {
    throw new TypeError('Direct ZIP recovery gate escaped its authoritative checkpoint')
  }
  return Object.freeze({
    id: directZipStateId(checkpoint.operationId),
    schemaVersion: DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
    operationId: checkpoint.operationId,
    leaseId,
    checkpointGeneration: checkpoint.generation.toString(10),
    checkpointDigest: checkpoint.digest,
    checkpoint,
    ...(recoveryGate === undefined ? {} : { recoveryGate }),
  })
}

export async function validateDirectZipStateRowV1(
  input: DirectZipStateRowV1,
): Promise<DirectZipStateRowV1> {
  requireJournalVersion(input.schemaVersion)
  const rebuilt = await createDirectZipStateRowV1(
    input.checkpoint,
    input.leaseId,
    input.recoveryGate,
  )
  if (input.id !== rebuilt.id || input.operationId !== rebuilt.operationId ||
      input.checkpointGeneration !== rebuilt.checkpointGeneration ||
      input.checkpointDigest !== rebuilt.checkpointDigest) {
    throw new TypeError('Direct ZIP state row projections disagree')
  }
  return rebuilt
}
export {
  createDirectZipMemberEntryPlanEvidenceV2,
} from './records/member-resume'
export {
  createDirectZipTargetObservationV1,
  validateDirectZipTargetObservationV1,
  type DirectZipTargetObservationInputV1,
} from './records/target-observation'
export {
  directZipCandidateId,
  directZipPageId,
  directZipPageKindByte,
  directZipStateId,
} from './records/canonical-fields'
