import { decodeBase64Url, encodeBase64Url } from '../../../../crypto/bytes'
import {
  chainDirectZipEpochDigestV1,
  digestDirectZipArchiveBytes,
  encodeDirectZipLocalHeaderV2,
  requireDirectZipFsaOffset,
  validateDirectZipEntryPlanV2,
} from '../../format'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalU8,
  canonicalU32,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../../../workspace/canonical'
import { validateDirectZipJournalBudgetUsageV1 } from '../budget'
import type {
  DirectZipCheckpointPhase,
  DirectZipCheckpointV1,
  DirectZipMemberResumeV1,
  DirectZipPageChainV1,
} from '../model'
import {
  canonicalBudgetUsage,
  canonicalPageChain,
  digestFrame,
  fixedFrame,
  identityFrame,
  optionalTextFrame,
  optionalCanonicalText,
  snapshotDigest,
  snapshotFixedBase64,
  snapshotPageChain,
  requireU64,
} from './canonical-fields'

const DIRECT_ZIP_MEMBER_ENTRY_PLAN_DOMAIN = 'windshare/direct-zip-member-entry-plan/v2'

export async function snapshotCurrentMember(
  input: DirectZipMemberResumeV1 | undefined,
  phase: DirectZipCheckpointPhase,
): Promise<DirectZipMemberResumeV1 | undefined> {
  if ((phase === 'inside-member') !== (input !== undefined)) {
    throw new TypeError('inside-member checkpoint authority must include exactly one member resume state')
  }
  if (input === undefined) return undefined
  const exactSize = requireU64(input.exactSize, 'member exact size')
  const memberPayloadOffset = requireU64(input.memberPayloadOffset, 'member payload offset')
  if (memberPayloadOffset > exactSize) throw new TypeError('member payload offset exceeds exact size')
  if (!Number.isInteger(input.crc32Accumulator) || input.crc32Accumulator < 0 ||
      input.crc32Accumulator > 0xffff_ffff) {
    throw new TypeError('member CRC32 accumulator is invalid')
  }
  const entryPlan = validateDirectZipEntryPlanV2(input.entryPlan)
  const entryPlanCanonicalBytes = canonicalMemberEntryPlan(entryPlan)
  if (!equalCanonicalBytes(entryPlanCanonicalBytes, input.entryPlanCanonicalBytes)) {
    throw new TypeError('member entry plan canonical bytes disagree with its plan')
  }
  const entryPlanDigest = snapshotDigest(input.entryPlanDigest, 'member entry plan digest')
  if (entryPlanDigest !== await canonicalDigest(entryPlanCanonicalBytes)) {
    throw new TypeError('member entry plan digest disagrees with its canonical bytes')
  }
  return Object.freeze({
    fileId: snapshotIdentity(input.fileId, 16, 'file ID'),
    fileRevision: snapshotIdentity(input.fileRevision, 16, 'file revision'),
    exactSize,
    sourceRangeAuthorityDigest: snapshotDigest(
      input.sourceRangeAuthorityDigest,
      'source range authority digest',
    ),
    entryPlan,
    entryPlanCanonicalBytes,
    entryPlanDigest,
    memberPayloadOffset,
    crc32Accumulator: input.crc32Accumulator,
    rollback: snapshotMemberRollback(input.rollback),
  })
}

function canonicalMemberEntryPlan(
  input: ReturnType<typeof validateDirectZipEntryPlanV2>,
): CanonicalBytes {
  return canonicalRecord(DIRECT_ZIP_MEMBER_ENTRY_PLAN_DOMAIN, 2, [
    canonicalFrame(canonicalU64(input.ordinal)),
    canonicalFrame(encodeDirectZipLocalHeaderV2(input)),
    canonicalFrame(canonicalU64(input.localHeaderBytes)),
    canonicalFrame(canonicalU64(input.descriptorBytes)),
    canonicalFrame(canonicalU64(input.entryStreamBytes)),
    canonicalFrame(canonicalU64(input.centralRecordBytes)),
  ])
}

export async function createDirectZipMemberEntryPlanEvidenceV2(
  input: Parameters<typeof validateDirectZipEntryPlanV2>[0],
): Promise<Readonly<{ canonicalBytes: CanonicalBytes; digest: string }>> {
  const canonicalBytes = canonicalMemberEntryPlan(validateDirectZipEntryPlanV2(input))
  return Object.freeze({ canonicalBytes, digest: await canonicalDigest(canonicalBytes) })
}

export function canonicalCurrentMember(input: DirectZipMemberResumeV1 | undefined): CanonicalBytes {
  if (input === undefined) return canonicalU8(1)
  return concatCanonicalBytes([
    canonicalU8(2),
    identityFrame(input.fileId, 16, 'file ID'),
    identityFrame(input.fileRevision, 16, 'file revision'),
    canonicalFrame(canonicalU64(input.exactSize)),
    digestFrame(input.sourceRangeAuthorityDigest, 'source range authority digest'),
    canonicalFrame(input.entryPlanCanonicalBytes),
    digestFrame(input.entryPlanDigest, 'member entry plan digest'),
    canonicalFrame(canonicalU64(input.memberPayloadOffset)),
    canonicalFrame(canonicalU32(input.crc32Accumulator)),
    canonicalFrame(canonicalMemberRollback(input.rollback)),
  ])
}

function snapshotMemberRollback(
  input: DirectZipMemberResumeV1['rollback'],
): DirectZipMemberResumeV1['rollback'] {
  requireDirectZipFsaOffset(input.archiveOffset, 'member rollback archive offset')
  const safeSelectedPayloadBytes = requireU64(
    input.safeSelectedPayloadBytes,
    'member rollback safe selected payload bytes',
  )
  const entryOrdinal = requireU64(input.entryOrdinal, 'member rollback entry ordinal')
  requireDirectZipFsaOffset(input.epochStart, 'member rollback epoch start')
  if (input.epochStart > input.archiveOffset) {
    throw new TypeError('member rollback epoch starts after its archive boundary')
  }
  const predecessorEpochRootDigest = snapshotFixedBase64(
    input.predecessorEpochRootDigest,
    32,
    'member rollback predecessor epoch root',
    true,
  )
  const epochContentDigest = snapshotFixedBase64(
    input.epochContentDigest,
    32,
    'member rollback epoch content digest',
    true,
  )
  const epochRootDigest = snapshotFixedBase64(
    input.epochRootDigest,
    32,
    'member rollback epoch root',
    true,
  )
  const derivedEpochRoot = input.epochStart === input.archiveOffset
    ? predecessorEpochRootDigest
    : encodeBase64Url(chainDirectZipEpochDigestV1({
        predecessorRoot: decodeBase64Url(predecessorEpochRootDigest)!,
        start: input.epochStart,
        end: input.archiveOffset,
        contentDigest: decodeBase64Url(epochContentDigest)!,
      }))
  if (input.epochStart === input.archiveOffset &&
      epochContentDigest !== encodeBase64Url(digestDirectZipArchiveBytes(new Uint8Array()))) {
    throw new TypeError('empty member rollback epoch has a non-empty content digest')
  }
  if (epochRootDigest !== derivedEpochRoot) {
    throw new TypeError('member rollback epoch root disagrees with its range authority')
  }
  const layoutPages = snapshotPageChain(input.layoutPages, 'member rollback layout')
  const centralPages = snapshotPageChain(input.centralPages, 'member rollback central')
  const epochPages = snapshotPageChain(input.epochPages, 'member rollback epoch')
  const journalUsage = validateDirectZipJournalBudgetUsageV1(input.journalUsage)
  if (journalUsage.memberCount !== layoutPages.recordCount ||
      journalUsage.canonicalMetadataBytes !== layoutPages.canonicalMetadataBytes +
        centralPages.canonicalMetadataBytes + epochPages.canonicalMetadataBytes ||
      layoutPages.recordCount !== entryOrdinal || centralPages.recordCount !== entryOrdinal) {
    throw new TypeError('member rollback pages disagree with their accounting boundary')
  }
  const accountingTailPageId = optionalCanonicalText(
    input.accountingTailPageId,
    'member rollback accounting tail page ID',
  )
  if ((accountingTailPageId === undefined) !==
      (journalUsage.memberCount === 0n && journalUsage.canonicalMetadataBytes === 0n)) {
    throw new TypeError('member rollback accounting tail disagrees with its journal usage')
  }
  return Object.freeze({
    archiveOffset: input.archiveOffset,
    safeSelectedPayloadBytes,
    entryOrdinal,
    epochStart: input.epochStart,
    predecessorEpochRootDigest,
    epochContentDigest,
    epochRootDigest,
    layoutPages,
    centralPages,
    epochPages,
    journalUsage,
    ...(accountingTailPageId === undefined ? {} : { accountingTailPageId }),
  })
}

function canonicalMemberRollback(input: DirectZipMemberResumeV1['rollback']): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalFrame(canonicalU64(input.archiveOffset)),
    canonicalFrame(canonicalU64(input.safeSelectedPayloadBytes)),
    canonicalFrame(canonicalU64(input.entryOrdinal)),
    canonicalFrame(canonicalU64(input.epochStart)),
    fixedFrame(input.predecessorEpochRootDigest, 32, 'member rollback predecessor epoch root', true),
    fixedFrame(input.epochContentDigest, 32, 'member rollback epoch content digest', true),
    fixedFrame(input.epochRootDigest, 32, 'member rollback epoch root', true),
    canonicalFrame(canonicalPageChain(input.layoutPages)),
    canonicalFrame(canonicalPageChain(input.centralPages)),
    canonicalFrame(canonicalPageChain(input.epochPages)),
    canonicalFrame(canonicalBudgetUsage(input.journalUsage)),
    optionalTextFrame(input.accountingTailPageId),
  ])
}

function validateCurrentMemberCheckpointAuthority(
  member: DirectZipMemberResumeV1,
  entryOrdinal: bigint,
  archiveOffset: bigint,
  committedSelectedPayloadBytes: bigint,
  layoutPages: DirectZipPageChainV1,
  centralPages: DirectZipPageChainV1,
  epochPages: DirectZipPageChainV1,
): void {
  const expectedArchiveOffset = member.entryPlan.zipEntry.localHeaderOffset +
    member.entryPlan.localHeaderBytes + member.memberPayloadOffset
  if (member.rollback.entryOrdinal !== entryOrdinal ||
      member.entryPlan.ordinal !== entryOrdinal || member.entryPlan.zipEntry.kind !== 'file' ||
      member.entryPlan.zipEntry.exactSize !== member.exactSize ||
      member.entryPlan.zipEntry.localHeaderOffset !== member.rollback.archiveOffset ||
      expectedArchiveOffset !== archiveOffset ||
      member.rollback.archiveOffset > archiveOffset ||
      member.rollback.safeSelectedPayloadBytes > committedSelectedPayloadBytes ||
      member.rollback.safeSelectedPayloadBytes + member.memberPayloadOffset !==
        committedSelectedPayloadBytes ||
      layoutPages.recordCount !== entryOrdinal + 1n ||
      centralPages.recordCount !== entryOrdinal ||
      !sameRollbackPagePrefix(member.rollback.layoutPages, layoutPages, true) ||
      !sameRollbackPagePrefix(member.rollback.centralPages, centralPages, false) ||
      !sameRollbackPagePrefix(member.rollback.epochPages, epochPages, true)) {
    throw new TypeError('current member rollback boundary disagrees with checkpoint progress')
  }
}

export function validateCheckpointRecoveryAuthority(input: Readonly<{
  currentMember: DirectZipMemberResumeV1 | undefined
  entryOrdinal: bigint
  archiveOffset: bigint
  committedSelectedPayloadBytes: bigint
  layoutPages: DirectZipPageChainV1
  centralPages: DirectZipPageChainV1
  epochPages: DirectZipPageChainV1
  closingReplay: DirectZipCheckpointV1['closingReplay']
  committedArchiveLength: bigint
}>): void {
  if (input.currentMember !== undefined) {
    validateCurrentMemberCheckpointAuthority(
      input.currentMember,
      input.entryOrdinal,
      input.archiveOffset,
      input.committedSelectedPayloadBytes,
      input.layoutPages,
      input.centralPages,
      input.epochPages,
    )
  }
  if (input.closingReplay?.completion !== undefined &&
      input.closingReplay.completion.exactArchiveBytes !== input.committedArchiveLength) {
    throw new TypeError('Direct ZIP completion length disagrees with committed authority')
  }
}

function sameRollbackPagePrefix(
  rollback: DirectZipPageChainV1,
  current: DirectZipPageChainV1,
  requiresFreshPage: boolean,
): boolean {
  if (rollback.chainId !== current.chainId || rollback.pageCount > current.pageCount ||
      rollback.recordCount > current.recordCount ||
      rollback.canonicalMetadataBytes > current.canonicalMetadataBytes) return false
  if (requiresFreshPage) return rollback.pageCount < current.pageCount
  return rollback.pageCount === current.pageCount && rollback.rootDigest === current.rootDigest &&
    rollback.recordCount === current.recordCount &&
    rollback.canonicalMetadataBytes === current.canonicalMetadataBytes
}
