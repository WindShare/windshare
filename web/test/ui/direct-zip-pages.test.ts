import { describe, expect, it } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  chainDirectZipEpochDigestV1, digestDirectZipArchiveBytes, directZipEpochGenesisRoot, planDirectZipEntryV2,
} from '../../src/output/direct-zip/format'
import {
  createDirectZipCheckpointV1, createDirectZipImmutablePageV1, createDirectZipMemberEntryPlanEvidenceV2,
  createDirectZipTargetObservationV1, directZipJournalBudgetDigestV1,
  type DirectZipImmutablePageV1, type DirectZipJournalRepository,
} from '../../src/output/direct-zip/journal'
import type { DirectZipEpochProofV1 } from '../../src/output/direct-zip/writer'
import { chainForKind, chainFromPage } from '../../src/ui/browser-receive/direct-zip/journal/bootstrap'
import { BrowserDirectZipPages, pageState, type BrowserDirectZipPageAuthority } from '../../src/ui/browser-receive/direct-zip/journal/pages'
import { encodeCentral, encodeEpoch, encodeLayout } from '../../src/ui/browser-receive/direct-zip/journal/records'

const identity = (width: number, fill: number) => encodeBase64Url(new Uint8Array(width).fill(fill))
const OPERATION_ID = identity(16, 1)
const LEASE_ID = identity(16, 2)
const ZERO_DIGEST = identity(32, 0)

async function collect<T>(values: AsyncIterable<T>): Promise<T[]> {
  const result: T[] = []
  for await (const value of values) result.push(value)
  return result
}

function proof(start: bigint, end: bigint, predecessorRoot: Uint8Array): DirectZipEpochProofV1 {
  const contentDigest = digestDirectZipArchiveBytes(new Uint8Array(Number(end - start)).fill(Number(start)))
  return { start, end, predecessorRoot, contentDigest,
    epochRoot: chainDirectZipEpochDigestV1({ start, end, predecessorRoot, contentDigest }) }
}

describe('Direct ZIP retained member-prefix proof pages', () => {
  it('retains a shorter terminal proof without overwriting a committed immutable epoch', async () => {
    const fixture = await createFixture()
    const pages = await fixture.open()
    const authority = await pages.prepareMemberRollback(fixture.checkpoint)
    expect(fixture.staged).toHaveLength(0)
    expect(fixture.rows.get(fixture.oldSuffix.id)).toBe(fixture.oldSuffix)
    expect(authority.epochPages).toEqual(fixture.rollback.epochPages)
    expect(authority.journalUsage).toEqual(fixture.rollback.journalUsage)
    expect(authority.retainedEpochProof?.end).toBe(fixture.rollback.archiveOffset)
    const cut = { operationId: OPERATION_ID, committedLength: fixture.retained.end, epochRoot: fixture.retained.epochRoot }
    for (let attempt = 0; attempt < 2; attempt++) {
      const proofs = await collect(pages.epochProofsFor(authority, cut))
      expect(proofs).toEqual([fixture.first, fixture.retained])
    }
    await expect(collect(pages.epochProofsFor({
      ...authority,
      retainedEpochProof: { ...authority.retainedEpochProof!, start: 11n },
    }, cut))).rejects.toThrow('epoch proof is inconsistent')
    await expect(collect(pages.epochProofsFor(authority, { ...cut, committedLength: cut.committedLength + 1n })))
      .rejects.toThrow('disagree with committed target bytes')
  })

  it('reopens the rollback cut and materializes its proof before recording the next member snapshot', async () => {
    const fixture = await createFixture()
    const previous = await fixture.open()
    const authority = await previous.prepareMemberRollback(fixture.checkpoint)
    await fixture.promote(authority)
    const pages = await fixture.open()
    expect(await collect(pages.epochProofsFor(pages.committedAuthority, fixture.cut))).toEqual([
      fixture.first, fixture.retained,
    ])
    const beforeAdmission = await pages.snapshot()
    await pages.stageLayout(fixture.admission)
    expect(fixture.staged.map(page => page.pageKind)).toEqual(['epoch', 'layout'])
    expect(fixture.rows.get(fixture.oldSuffix.id)).toBe(fixture.staged[0])
    const materialized = fixture.staged[0]!
    expect(materialized.accountingPredecessor).toEqual({ kind: 'checkpoint',
      checkpointGeneration: fixture.checkpoint.generation, checkpointDigest: fixture.checkpoint.digest })
    expect(materialized.budgetUsage.canonicalMetadataBytes).toBe(
      authority.journalUsage.canonicalMetadataBytes + BigInt(materialized.canonicalBytes.byteLength),
    )
    const rollback = pages.rollbackAuthority(beforeAdmission)
    expect(rollback.retainedEpochProof).toBeUndefined()
    expect(rollback.epochPages.pageCount).toBe(authority.epochPages.pageCount + 1n)
    expect(pages.authority.retainedEpochProof).toBeUndefined()
    const next = proof(fixture.retained.end, fixture.retained.end + 100n, fixture.retained.epochRoot)
    await pages.stageEpoch(next)
    expect(await collect(pages.epochProofsFor(pages.authority, {
      operationId: OPERATION_ID, committedLength: next.end, epochRoot: next.epochRoot,
    }))).toEqual([fixture.first, fixture.retained, next])
  })

  it('preserves the materialized prefix after another source change and durable member checkpoint', async () => {
    const fixture = await createFixture()
    const original = await fixture.open()
    await fixture.promote(await original.prepareMemberRollback(fixture.checkpoint))
    const resumed = await fixture.open()
    const beforeAdmission = await resumed.snapshot()
    await resumed.stageLayout(fixture.admission)
    const memberEpoch = proof(fixture.retained.end, fixture.memberEnd, fixture.retained.epochRoot)
    await resumed.stageEpoch(memberEpoch)
    await fixture.commitMember(resumed.authority, resumed.rollbackAuthority(beforeAdmission), memberEpoch)

    const afterPause = await fixture.open()
    expect(await collect(afterPause.epochProofsFor(afterPause.committedAuthority, {
      operationId: OPERATION_ID, committedLength: memberEpoch.end, epochRoot: memberEpoch.epochRoot,
    }))).toEqual([fixture.first, fixture.retained, memberEpoch])
    const nextRollback = await afterPause.prepareMemberRollback(fixture.checkpoint)
    expect(nextRollback.retainedEpochProof).toBeUndefined()
    expect(nextRollback.epochPages.pageCount).toBe(2n)
    await fixture.promote(nextRollback)
    const afterSecondRollback = await fixture.open()
    expect(await collect(afterSecondRollback.epochProofsFor(afterSecondRollback.committedAuthority, fixture.cut)))
      .toEqual([fixture.first, fixture.retained])
    expect(fixture.checkpoint.committedSelectedPayloadBytes).toBe(0n)
    expect(fixture.checkpoint.currentMember).toBeUndefined()
  })

  it('discards an interrupted materialization and can replay the durable inline proof again', async () => {
    const fixture = await createFixture()
    const original = await fixture.open()
    await fixture.promote(await original.prepareMemberRollback(fixture.checkpoint))
    const pages = await fixture.open()
    const committed = pageState(pages.committedAuthority)
    fixture.rejectLayout = true
    await expect(pages.stageLayout(fixture.admission)).rejects.toThrow('injected layout staging failure')
    expect(fixture.staged.map(page => page.pageKind)).toEqual(['epoch'])
    await pages.restore(committed)
    expect(fixture.rows.has(fixture.oldSuffix.id)).toBe(false)
    const reopened = await fixture.open()
    expect(await collect(reopened.epochProofsFor(reopened.committedAuthority, fixture.cut))).toEqual([
      fixture.first, fixture.retained,
    ])
    fixture.rejectLayout = false
    await reopened.stageLayout(fixture.admission)
    expect(reopened.authority.retainedEpochProof).toBeUndefined()
  })
})

async function createFixture() {
  const first = proof(0n, 10n, directZipEpochGenesisRoot())
  const retained = proof(first.end, 15n, first.epochRoot)
  const rootLayout = await createDirectZipImmutablePageV1({
    operationId: OPERATION_ID, pageKind: 'layout', chainId: identity(16, 3), pageOrdinal: 0,
    predecessorRootDigest: ZERO_DIGEST, canonicalEntries: [Uint8Array.of(1)],
    accountingPredecessor: { kind: 'checkpoint', checkpointGeneration: 1n, checkpointDigest: identity(32, 6) },
    previousBudgetUsage: { memberCount: 0n, canonicalMetadataBytes: 0n },
    previousChainRecordCount: 0n, previousChainCanonicalMetadataBytes: 0n,
  })
  const rootCentral = await createDirectZipImmutablePageV1({
    operationId: OPERATION_ID, pageKind: 'central', chainId: identity(16, 4), pageOrdinal: 0,
    predecessorRootDigest: ZERO_DIGEST, canonicalEntries: [encodeCentral(0n, Uint8Array.of(2))],
    accountingPredecessor: { kind: 'page', pageKind: rootLayout.pageKind, pageId: rootLayout.id, pageDigest: rootLayout.digest },
    previousBudgetUsage: rootLayout.budgetUsage, previousChainRecordCount: 0n, previousChainCanonicalMetadataBytes: 0n,
  })
  const initial = await createDirectZipImmutablePageV1({
    operationId: OPERATION_ID, pageKind: 'epoch', chainId: identity(16, 5), pageOrdinal: 0,
    predecessorRootDigest: ZERO_DIGEST, canonicalEntries: [encodeEpoch(first)],
    accountingPredecessor: { kind: 'page', pageKind: rootCentral.pageKind, pageId: rootCentral.id, pageDigest: rootCentral.digest },
    previousBudgetUsage: rootCentral.budgetUsage,
    previousChainRecordCount: 0n, previousChainCanonicalMetadataBytes: 0n,
  })
  const rollback = {
    archiveOffset: retained.end, safeSelectedPayloadBytes: 0n, entryOrdinal: 1n, epochStart: retained.start,
    predecessorEpochRootDigest: encodeBase64Url(retained.predecessorRoot),
    epochContentDigest: encodeBase64Url(retained.contentDigest), epochRootDigest: encodeBase64Url(retained.epochRoot),
    layoutPages: chainFromPage(rootLayout), centralPages: chainFromPage(rootCentral), epochPages: chainFromPage(initial),
    journalUsage: initial.budgetUsage, accountingTailPageId: initial.id,
  }
  const plan = planDirectZipEntryV2({ ordinal: 1n, localHeaderOffset: retained.end,
    entry: { kind: 'file', path: ['root', 'file.bin'], exactSize: 5n } })
  const planEvidence = await createDirectZipMemberEntryPlanEvidenceV2(plan)
  const admission = { plan, layoutEvidence: Uint8Array.of(1), discoveryEvidence: Uint8Array.of(2),
    source: { fileId: identity(16, 7), revision: identity(16, 8), exactSize: 5n, rangeAuthority: identity(32, 9) } }
  const layout = await createDirectZipImmutablePageV1({
    operationId: OPERATION_ID, pageKind: 'layout', chainId: rollback.layoutPages.chainId, pageOrdinal: 1,
    predecessorRootDigest: rootLayout.chainRootDigest, canonicalEntries: [encodeLayout(admission)],
    accountingPredecessor: { kind: 'page', pageKind: initial.pageKind, pageId: initial.id, pageDigest: initial.digest },
    previousBudgetUsage: initial.budgetUsage, previousChainRecordCount: rootLayout.chainRecordCount,
    previousChainCanonicalMetadataBytes: rootLayout.chainCanonicalMetadataBytes,
  })
  const discarded = proof(first.end, plan.zipEntry.localHeaderOffset + plan.localHeaderBytes + 3n, first.epochRoot)
  const oldSuffix = await createDirectZipImmutablePageV1({
    operationId: OPERATION_ID, pageKind: 'epoch', chainId: initial.chainId, pageOrdinal: 1,
    predecessorRootDigest: initial.chainRootDigest, canonicalEntries: [encodeEpoch(discarded)],
    accountingPredecessor: { kind: 'page', pageKind: layout.pageKind, pageId: layout.id, pageDigest: layout.digest },
    previousBudgetUsage: layout.budgetUsage, previousChainRecordCount: initial.chainRecordCount,
    previousChainCanonicalMetadataBytes: initial.chainCanonicalMetadataBytes,
  })
  const checkpoint = await createDirectZipCheckpointV1({
    operationId: OPERATION_ID, receiveIntentDigest: identity(32, 10), targetBindingDigest: identity(32, 11),
    policies: { encodingPolicyDigest: identity(32, 12), layoutPolicyDigest: identity(32, 13),
      checkpointPolicyDigest: identity(32, 14), journalBudgetDigest: await directZipJournalBudgetDigestV1(),
      epochPolicyDigest: identity(32, 15) },
    generation: 2n, phase: 'inside-member', entryOrdinal: 1n,
    currentMember: { fileId: admission.source.fileId, fileRevision: admission.source.revision, exactSize: 5n,
      sourceRangeAuthorityDigest: admission.source.rangeAuthority, entryPlan: plan,
      entryPlanCanonicalBytes: planEvidence.canonicalBytes, entryPlanDigest: planEvidence.digest,
      memberPayloadOffset: 3n, crc32Accumulator: 0, rollback },
    discovery: { cursorCanonicalBytes: Uint8Array.of(1),
      directoryAdmissionDigest: identity(32, 16), discoveryRootDigest: identity(32, 17) },
    archiveOffset: discarded.end, committedArchiveLength: discarded.end, committedSelectedPayloadBytes: 3n,
    parentBindingDigest: identity(32, 18), fileBindingDigest: identity(32, 19),
    targetObservation: await observation(discarded), epochRootDigest: encodeBase64Url(discarded.epochRoot),
    layoutPages: chainFromPage(layout), centralPages: rollback.centralPages, epochPages: chainFromPage(oldSuffix),
    journalUsage: oldSuffix.budgetUsage, accountingTailPageId: oldSuffix.id,
  })
  const fixture = {
    checkpoint, first, retained, rollback, admission, oldSuffix, rejectLayout: false, memberEnd: discarded.end,
    rows: new Map([rootLayout, rootCentral, initial, layout, oldSuffix].map(page => [page.id, page])),
    staged: [] as DirectZipImmutablePageV1[],
    cut: { operationId: OPERATION_ID, committedLength: retained.end, epochRoot: retained.epochRoot },
    async open() {
      return BrowserDirectZipPages.open(repository, () => ({ operationId: OPERATION_ID,
        leaseId: LEASE_ID, checkpointGeneration: fixture.checkpoint.generation }), fixture.checkpoint)
    },
    async commitMember(
      authority: BrowserDirectZipPageAuthority, saved: BrowserDirectZipPageAuthority, epoch: DirectZipEpochProofV1,
    ) {
      fixture.checkpoint = await createDirectZipCheckpointV1({ ...checkpoint, ...authority,
        generation: fixture.checkpoint.generation + 1n, archiveOffset: epoch.end, committedArchiveLength: epoch.end,
        currentMember: { ...checkpoint.currentMember!, fileRevision: identity(16, 23), rollback: {
          ...rollback, ...saved, epochStart: retained.end, predecessorEpochRootDigest: encodeBase64Url(retained.epochRoot),
          epochContentDigest: encodeBase64Url(digestDirectZipArchiveBytes(new Uint8Array())),
        } }, epochRootDigest: encodeBase64Url(epoch.epochRoot), targetObservation: await observation(epoch) })
    },
    async promote(authority: BrowserDirectZipPageAuthority) {
      const { currentMember, ...previous } = fixture.checkpoint
      expect(currentMember).toBeDefined()
      fixture.checkpoint = await createDirectZipCheckpointV1({ ...previous, ...authority,
        generation: previous.generation + 1n, phase: 'between-members', archiveOffset: retained.end,
        committedArchiveLength: retained.end, committedSelectedPayloadBytes: rollback.safeSelectedPayloadBytes,
        epochRootDigest: encodeBase64Url(retained.epochRoot), targetObservation: await observation(retained) })
    },
  }
  const repository = {
    async *streamPages(scan: { pageKind: string; chainId: string }) {
      for (const page of [...fixture.rows.values()].sort((a, b) => a.pageOrdinal - b.pageOrdinal)) {
        if (page.pageKind === scan.pageKind && page.chainId === scan.chainId) yield page
      }
    },
    async readState() { return { checkpointDigest: fixture.checkpoint.digest } },
    async stagePage(_fence: unknown, page: DirectZipImmutablePageV1) {
      if (fixture.rejectLayout && page.pageKind === 'layout') throw new Error('injected layout staging failure')
      const previous = fixture.rows.get(page.id)
      if (previous !== undefined && previous.digest !== page.digest) throw new Error('immutable page conflict')
      fixture.rows.set(page.id, page)
      fixture.staged.push(page)
    },
    async collectOrphanPages() {
      for (const page of fixture.rows.values()) {
        const chain = chainForKind(fixture.checkpoint, page.pageKind)
        if (page.chainId !== chain.chainId || BigInt(page.pageOrdinal) >= chain.pageCount) fixture.rows.delete(page.id)
      }
    },
  } as unknown as DirectZipJournalRepository
  return fixture
}

async function observation(epoch: DirectZipEpochProofV1) {
  return createDirectZipTargetObservationV1({ operationId: OPERATION_ID,
    parentBindingDigest: identity(32, 18), fileBindingDigest: identity(32, 19),
    ownershipMarkerDigest: identity(32, 20), exactLength: epoch.end, lastModifiedMilliseconds: 1,
    epochRootDigest: encodeBase64Url(epoch.epochRoot) })
}
