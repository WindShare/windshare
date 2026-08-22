import { describe, expect, it } from 'vitest'
import { encodeBase64Url } from '../../../../src/crypto/bytes'
import {
  digestDirectZipArchiveBytes,
  planDirectZipEntryV2,
} from '../../../../src/output/direct-zip/format'
import { directZipJournalBudgetDigestV1 } from '../../../../src/output/direct-zip/journal/budget'
import {
  createDirectZipCheckpointV1,
  createDirectZipMemberEntryPlanEvidenceV2,
  createDirectZipTargetObservationV1,
} from '../../../../src/output/direct-zip/journal/records'

const ZERO_DIGEST = identity(32, 0)

describe('Direct ZIP canonical checkpoint authority', () => {
  it('persists the active member plan, source/CRC state, and complete pre-admission rollback', async () => {
    const operationId = identity(16, 1)
    const parentBindingDigest = identity(32, 2)
    const fileBindingDigest = identity(32, 3)
    const finalEpochRoot = identity(32, 4)
    const plan = planDirectZipEntryV2({
      ordinal: 1n,
      localHeaderOffset: 100n,
      entry: { kind: 'file', path: ['root', 'file.bin'], exactSize: 5n },
    })
    const planEvidence = await createDirectZipMemberEntryPlanEvidenceV2(plan)
    const checkpointArchiveOffset = 100n + plan.localHeaderBytes + 3n
    const targetObservation = await createDirectZipTargetObservationV1({
      operationId,
      parentBindingDigest,
      fileBindingDigest,
      ownershipMarkerDigest: identity(32, 5),
      exactLength: checkpointArchiveOffset,
      lastModifiedMilliseconds: 1,
      epochRootDigest: finalEpochRoot,
    })
    const checkpoint = await createDirectZipCheckpointV1({
      operationId,
      receiveIntentDigest: identity(32, 6),
      targetBindingDigest: identity(32, 7),
      policies: await policies(),
      generation: 2n,
      predecessorCheckpointDigest: identity(32, 8),
      phase: 'inside-member',
      entryOrdinal: 1n,
      currentMember: {
        fileId: identity(16, 9),
        fileRevision: identity(16, 10),
        exactSize: 5n,
        sourceRangeAuthorityDigest: identity(32, 11),
        entryPlan: plan,
        entryPlanCanonicalBytes: planEvidence.canonicalBytes,
        entryPlanDigest: planEvidence.digest,
        memberPayloadOffset: 3n,
        crc32Accumulator: 0x1234_5678,
        rollback: {
          archiveOffset: 100n,
          safeSelectedPayloadBytes: 0n,
          entryOrdinal: 1n,
          epochStart: 100n,
          predecessorEpochRootDigest: identity(32, 12),
          epochContentDigest: encodeBase64Url(digestDirectZipArchiveBytes(new Uint8Array())),
          epochRootDigest: identity(32, 12),
          layoutPages: chain(13, 1n, 10n),
          centralPages: chain(14, 1n, 20n),
          epochPages: emptyChain(15),
          journalUsage: { memberCount: 1n, canonicalMetadataBytes: 30n },
          accountingTailPageId: 'rollback-tail',
        },
      },
      discovery: {
        cursorCanonicalBytes: Uint8Array.of(1),
        directoryAdmissionDigest: identity(32, 16),
        discoveryRootDigest: identity(32, 17),
      },
      archiveOffset: checkpointArchiveOffset,
      committedArchiveLength: checkpointArchiveOffset,
      committedSelectedPayloadBytes: 3n,
      parentBindingDigest,
      fileBindingDigest,
      targetObservation,
      epochRootDigest: finalEpochRoot,
      layoutPages: {
        ...chain(13, 2n, 30n),
        rootDigest: identity(32, 18),
        pageCount: 2n,
      },
      centralPages: chain(14, 1n, 20n),
      epochPages: {
        ...chain(15, 1n, 10n),
        rootDigest: identity(32, 20),
      },
      journalUsage: { memberCount: 2n, canonicalMetadataBytes: 60n },
      accountingTailPageId: 'current-tail',
    })

    expect(checkpoint.currentMember).toEqual(expect.objectContaining({
      entryPlanDigest: planEvidence.digest,
      memberPayloadOffset: 3n,
      crc32Accumulator: 0x1234_5678,
      rollback: expect.objectContaining({ archiveOffset: 100n, entryOrdinal: 1n }),
    }))
    await expect(createDirectZipCheckpointV1({
      ...checkpoint,
      currentMember: {
        ...checkpoint.currentMember!,
        rollback: { ...checkpoint.currentMember!.rollback, archiveOffset: 101n },
      },
    })).rejects.toThrow('epoch root disagrees')
  })

  it('accepts terminal completion only when it binds the exact committed archive length', async () => {
    const operationId = identity(16, 21)
    const parentBindingDigest = identity(32, 22)
    const fileBindingDigest = identity(32, 23)
    const epochRootDigest = identity(32, 24)
    const observation = await createDirectZipTargetObservationV1({
      operationId,
      parentBindingDigest,
      fileBindingDigest,
      ownershipMarkerDigest: identity(32, 25),
      exactLength: 120n,
      lastModifiedMilliseconds: 2,
      epochRootDigest,
    })
    const input = {
      operationId,
      receiveIntentDigest: identity(32, 26),
      targetBindingDigest: identity(32, 27),
      policies: await policies(),
      generation: 3n,
      predecessorCheckpointDigest: identity(32, 28),
      candidateLineageDigest: identity(32, 29),
      phase: 'closing' as const,
      entryOrdinal: 1n,
      discovery: {
        cursorCanonicalBytes: Uint8Array.of(2),
        directoryAdmissionDigest: identity(32, 30),
        discoveryRootDigest: identity(32, 31),
      },
      archiveOffset: 120n,
      committedArchiveLength: 120n,
      committedSelectedPayloadBytes: 5n,
      parentBindingDigest,
      fileBindingDigest,
      targetObservation: observation,
      epochRootDigest,
      layoutPages: chain(32, 1n, 10n),
      centralPages: chain(33, 1n, 20n),
      epochPages: emptyChain(34),
      journalUsage: { memberCount: 1n, canonicalMetadataBytes: 30n },
      accountingTailPageId: 'closing-tail',
      closingReplay: {
        archiveOffset: 100n,
        centralRecordRootDigest: identity(32, 35),
        completion: {
          exactArchiveBytes: 120n,
          preClosingEpochRootDigest: identity(32, 36),
        },
      },
    }
    const checkpoint = await createDirectZipCheckpointV1(input)
    expect(checkpoint.closingReplay?.completion?.exactArchiveBytes).toBe(120n)
    await expect(createDirectZipCheckpointV1({
      ...input,
      closingReplay: {
        ...input.closingReplay,
        completion: { ...input.closingReplay.completion, exactArchiveBytes: 119n },
      },
    })).rejects.toThrow('completion length disagrees')
  })
})

function chain(fill: number, records: bigint, bytes: bigint) {
  return {
    chainId: identity(16, fill),
    rootDigest: identity(32, fill),
    pageCount: 1n,
    recordCount: records,
    canonicalMetadataBytes: bytes,
  }
}

function emptyChain(fill: number) {
  return {
    chainId: identity(16, fill),
    rootDigest: ZERO_DIGEST,
    pageCount: 0n,
    recordCount: 0n,
    canonicalMetadataBytes: 0n,
  }
}

async function policies() {
  return {
    encodingPolicyDigest: identity(32, 40),
    layoutPolicyDigest: identity(32, 41),
    checkpointPolicyDigest: identity(32, 42),
    journalBudgetDigest: await directZipJournalBudgetDigestV1(),
    epochPolicyDigest: identity(32, 43),
  }
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
