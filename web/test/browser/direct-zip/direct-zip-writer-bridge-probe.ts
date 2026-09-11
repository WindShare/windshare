import { encodeBase64Url, decodeBase64Url } from '../../../src/crypto/bytes'
import {
  encodeDirectZipBootstrapPrefixV1, planDirectZipEntryV2, snapshotDirectZipOwnershipMarkerV1,
} from '../../../src/output/direct-zip/format'
import {
  createDirectZipBootstrapCandidateV1, createDirectZipTargetObservationV1,
  directZipJournalBudgetDigestV1, IndexedDbDirectZipJournalRepository,
  type DirectZipJournalRepository,
} from '../../../src/output/direct-zip/journal'
import { DirectZipEpochWriterV1 } from '../../../src/output/direct-zip/writer'
import {
  createReceiveOperationV2, receiveOperationHandleRecord, receiveOperationLeaseRecord,
  storedReceiveOperationRecord,
} from '../../../src/output/workspace/records'
import { initialReceiveLifecycleState, nextReceiveLifecycleState } from '../../../src/output/workspace/state'
import { storedReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import {
  createDirectorySelectionResultRoot, createDirectResumableZipPlan, createFSAOwnedFileBinding,
  createReceiveIntent, createSelectionSpec, createZipArchiveArtifact, deriveArtifactChoiceIdentity,
} from '../../../src/transfer/intent'
import { BrowserDirectZipJournal, createInitialBrowserDirectZipCheckpoint } from '../../../src/ui/browser-receive/direct-zip/journal'
import {
  INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE, INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
  openIndexedDbCheckpointDatabase, requestResult, transactionCompletion,
} from '../../../src/output/browser/indexeddb-database'
import { StagedDirectZipTarget } from '../../output/direct-zip/writer/fault-model'

const identity = (width: number, fill: number) => encodeBase64Url(new Uint8Array(width).fill(fill))

export async function probeDirectZipWriterBridge(
  databaseName: string,
  rollbackFault?: 'before-publish' | 'unknown-tail',
) {
  const repository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
  const raw = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const operationId = identity(16, 1)
    const leaseId = identity(16, 2)
    const rootId = identity(16, 3)
    const candidateId = identity(16, 4)
    const rootComponent = 'shared'
    const parentBindingDigest = identity(32, 5)
    const fileBindingDigest = identity(32, 6)
    const selection = await createSelectionSpec({ shareInstance: identity(16, 7),
      syntheticRoot: rootId, rules: { mode: 'node-id', defaultSelected: true, rules: [] } })
    const artifact = await createZipArchiveArtifact(createDirectorySelectionResultRoot(rootId, rootComponent))
    const binding = await createFSAOwnedFileBinding({
      operationId, artifact, stableName: rootComponent + '.windshare-' + candidateId + '.zip',
      targetRef: identity(32, 8), policies: {
        zipEncoding: identity(32, 9), layout: identity(32, 10), checkpoint: identity(32, 11),
        journalBudget: await directZipJournalBudgetDigestV1(), epoch: identity(32, 12),
      },
    })
    const plan = await createDirectResumableZipPlan(artifact, binding)
    const intent = await createReceiveIntent({ selection, artifact, plan })
    const choice = await deriveArtifactChoiceIdentity(artifact, plan)
    const operation = await createReceiveOperationV2({ receiveIntent: intent, preClickRanking: [choice.id] })
    const parent = receiveOperationHandleRecord({ id: 'parent', operationId, kind: 1,
      authorityRef: parentBindingDigest, handle: { test: 'parent' } })
    const file = receiveOperationHandleRecord({ id: 'file', operationId, kind: 2,
      authorityRef: fileBindingDigest, handle: { test: 'file' } })
    const lease = receiveOperationLeaseRecord({ operationId, leaseId, acquiredAt: 1 })
    const candidate = await createDirectZipBootstrapCandidateV1({
      operationId, candidateId, leaseId, leaseGeneration: 1n, parentHandleId: parent.id,
      selectionCanonicalBytes: selection.canonicalBytes, artifactCanonicalBytes: artifact.canonicalBytes,
      choiceIdentityCanonicalBytes: choice.canonicalBytes, choiceId: choice.id, preClickRanking: [choice.id],
      stablePhysicalName: binding.stableName, ownershipNonce: identity(32, 13), targetBindingDigest: binding.digest,
      policies: { encodingPolicyDigest: binding.policies.zipEncoding, layoutPolicyDigest: binding.policies.layout,
        checkpointPolicyDigest: binding.policies.checkpoint, journalBudgetDigest: binding.policies.journalBudget,
        epochPolicyDigest: binding.policies.epoch },
    })
    const marker = snapshotDirectZipOwnershipMarkerV1({
      operationId: decodeBase64Url(operationId)!, candidateId: decodeBase64Url(candidateId)!,
      ownershipNonce: decodeBase64Url(candidate.ownershipNonce)!, bindingDigest: decodeBase64Url(binding.digest)!,
    })
    const target = new StagedDirectZipTarget(encodeDirectZipBootstrapPrefixV1(rootComponent, marker))
    let lastObservedRoot = new Uint8Array(32)
    const observeTarget = async (epochRoot: Uint8Array) => {
      lastObservedRoot = Uint8Array.from(epochRoot)
      return createDirectZipTargetObservationV1({
        operationId, parentBindingDigest, fileBindingDigest, ownershipMarkerDigest: identity(32, 14),
        exactLength: BigInt(target.visible.byteLength), lastModifiedMilliseconds: target.visible.byteLength,
        epochRootDigest: encodeBase64Url(epochRoot),
      })
    }
    const initial = await createInitialBrowserDirectZipCheckpoint({
      candidate, receiveIntentDigest: intent.digest, parentBindingDigest, fileBindingDigest,
      ownershipMarker: marker, rootComponent, expectedRootDirectoryId: rootId, observeTarget,
    })
    let lifecycle = nextReceiveLifecycleState(initialReceiveLifecycleState({
      operationId, receiveIntentDigest: intent.digest,
    }), { kind: 'receiving', activeLeaseId: leaseId })
    await repository.createBootstrapCandidate({ candidate, provisionalParentHandle: parent, lease })
    const cut = { candidate, operation, operationRecord: storedReceiveOperationRecord(operation),
      lifecycle, lifecycleRecord: await storedReceiveLifecycleState(lifecycle),
      handles: [parent, file], lease, ...initial }
    const inject = raw.transaction(INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE, 'readwrite')
    inject.objectStore(INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE).add(initial.pages[1])
    await transactionCompletion(inject)
    let bootstrapFault = false
    try { await repository.commitBootstrap(cut) } catch { bootstrapFault = true }
    const check = raw.transaction(INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE, 'readonly')
    const layoutCountAfterFault = await requestResult(check.objectStore(INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE).count())
    await transactionCompletion(check)
    const bootstrapStateAfterFault = await repository.readState(operationId)
    const remove = raw.transaction(INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE, 'readwrite')
    remove.objectStore(INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE).delete(initial.pages[1]!.id)
    await transactionCompletion(remove)
    await repository.commitBootstrap(cut)
    let prematurePublicationRejected = false
    const premature = nextReceiveLifecycleState(lifecycle, {
      kind: 'published', receiptDigest: identity(32, 26), cleanupState: 'clean',
    })
    try {
      await repository.commitRecoveryLifecycle({
        fence: { operationId, leaseId, checkpointGeneration: initial.checkpoint.generation },
        lifecycle: premature, lifecycleRecord: await storedReceiveLifecycleState(premature),
      })
    } catch { prematurePublicationRejected = true }
    let rejectPromotion = true
    const failingRepository = new Proxy(repository, {
      get(owner, key) {
        if (key === 'promoteCandidate') return async (...args: Parameters<DirectZipJournalRepository['promoteCandidate']>) => {
          if (rejectPromotion) { rejectPromotion = false; throw new Error('injected checkpoint cut failure') }
          return owner.promoteCandidate(...args)
        }
        const value = Reflect.get(owner, key)
        return typeof value === 'function' ? value.bind(owner) : value
      },
    })
    const options = {
      repository: failingRepository, checkpoint: initial.checkpoint, leaseId, expectedRootDirectoryId: rootId,
      observeTarget,
      lifecycleForCheckpoint: async () => {
        const next = nextReceiveLifecycleState(lifecycle, { kind: 'receiving', activeLeaseId: leaseId })
        return { lifecycle: next, lifecycleRecord: await storedReceiveLifecycleState(next) }
      },
      onCheckpointCommitted: (_: unknown, next: typeof lifecycle) => { lifecycle = next },
    }
    let journal = await BrowserDirectZipJournal.open(options)
    const originalObservation = target.observeCandidate.bind(target)
    target.observeCandidate = async (epoch, closed) => ({
      ...await originalObservation(epoch, closed),
      observationDigest: decodeBase64Url((await observeTarget(
        BigInt(target.visible.byteLength) === epoch.stagedEnd ? epoch.expectedEpochRoot : journal.checkpoint.epochRoot,
      )).digest)!,
    })
    const originalCompletion = target.readBoundedCompletionProof.bind(target)
    target.readBoundedCompletionProof = async input => ({
      ...await originalCompletion(input),
      observationDigest: decodeBase64Url((await observeTarget(lastObservedRoot)).digest)!,
    })
    const originalTruncate = target.truncateToPredecessor.bind(target)
    target.truncateToPredecessor = async checkpoint => ({
      ...await originalTruncate(checkpoint),
      observationDigest: decodeBase64Url((await observeTarget(checkpoint.epochRoot)).digest)!,
    })
    const writer = () => new DirectZipEpochWriterV1({
      context: { ownershipMarker: marker, rootComponent }, checkpoint: journal.checkpoint,
      pages: journal.pages, cuts: journal.cuts, target,
      identities: { nextEpochId: () => identity(16, 21), nextCandidateId: () => encodeBase64Url(crypto.getRandomValues(new Uint8Array(16))) },
    })
    let engine = writer()
    const root = { directoryId: rootId, generation: identity(16, 15), discoveryEvidence: Uint8Array.of(1, 2, 3) }
    await journal.verifyRoot(journal.checkpoint, root)
    const source = { fileId: identity(16, 16), revision: identity(16, 17), exactSize: 6n,
      rangeAuthority: identity(32, 18) }
    const admission = { plan: planDirectZipEntryV2({ ordinal: 1n,
      localHeaderOffset: journal.checkpoint.archiveOffset,
      entry: { kind: 'file', path: [rootComponent, 'data.txt'], exactSize: 6n } }),
      source, layoutEvidence: Uint8Array.of(4, 5), discoveryEvidence: Uint8Array.of(6, 7) }
    const discardedMember = await engine.beginFile(admission)
    await discardedMember.write(Uint8Array.of(0))
    target.closeFaults.push('before-publish')
    const discardedCut = await engine.pause()
    const retiredBeforePublish = discardedCut.kind === 'replay-required' &&
      (await repository.readOperationCandidate(operationId)) === undefined &&
      journal.checkpoint.generation === initial.checkpoint.generation
    const member = await engine.beginFile(admission)
    await member.write(Uint8Array.of(65, 66, 67))
    let promotionFailed = false
    try { await engine.pause() } catch { promotionFailed = true }
    const staged = await repository.readOperationCandidate(operationId)
    if (staged?.kind !== 'epoch') throw new Error('epoch candidate is absent')
    const attention = nextReceiveLifecycleState(lifecycle, { kind: 'needs-attention',
      reason: 'publication-unknown', lastVerifiedRecordDigest: journal.persistedCheckpoint.digest })
    await repository.commitRecoveryLifecycle({
      fence: { operationId, leaseId, checkpointGeneration: journal.persistedCheckpoint.generation },
      candidate: staged, lifecycle: attention, lifecycleRecord: await storedReceiveLifecycleState(attention),
    })
    lifecycle = attention
    journal = await BrowserDirectZipJournal.open({ ...options, checkpoint: (await repository.readState(operationId))!.checkpoint })
    engine = writer()
    if (journal.pendingCandidate === undefined) throw new Error('candidate was lost after close')
    await engine.recoverCandidate(journal.pendingCandidate)
    const resumedOffset = journal.checkpoint.member?.payloadOffset
    await journal.verifyRoot(journal.checkpoint, root)
    const resumed = engine.resumeFile(source)
    if (!('write' in resumed)) throw new Error('unchanged member could not resume')
    await resumed.write(Uint8Array.of(68, 69, 70))
    await resumed.close()
    const rollbackRecovery = rollbackFault === undefined ? undefined :
      await probeClosingRollback({ engine, journal, target, admission, source, rootComponent, rollbackFault })
    const pages = await journal.pages.snapshot()
    const beforeClosing = journal.persistedCheckpoint
    const completion = await engine.closeArchive({
      entryCount: 2n, centralDirectoryBytes: pages.centralBytes,
      layoutRoot: pages.layoutRoot, centralRoot: pages.centralRoot,
      predecessorEpochRoot: journal.checkpoint.epochRoot,
    })
    let staleCandidateRejected = false
    try {
      await repository.bindCandidate({ operationId, leaseId,
        checkpointGeneration: staged.predecessorCheckpointGeneration }, staged)
    } catch { staleCandidateRejected = true }
    const reopened = await BrowserDirectZipJournal.open({
      ...options, checkpoint: (await repository.readState(operationId))!.checkpoint,
    })
    const proofCount = []
    for await (const proof of reopened.pages.committedEpochProofs(reopened.checkpoint)) proofCount.push(proof)
    const published = nextReceiveLifecycleState(lifecycle, {
      kind: 'published', receiptDigest: identity(32, 27), cleanupState: 'clean',
    })
    await repository.commitRecoveryLifecycle({
      fence: { operationId, leaseId, checkpointGeneration: reopened.persistedCheckpoint.generation },
      lifecycle: published, lifecycleRecord: await storedReceiveLifecycleState(published),
    })
    return {
      prematurePublicationRejected, publishedCommitted: true, rollbackRecovery,
      bootstrapFault, layoutCountAfterFault, bootstrapStateAbsent: bootstrapStateAfterFault === undefined,
      retiredBeforePublish,
      promotionFailed, candidateDurable: staged?.kind === 'epoch', resumedOffset: resumedOffset?.toString(),
      completionBytes: completion.exactArchiveBytes.toString(), storedCompletion: reopened.checkpoint.completion !== undefined,
      epochCount: proofCount.length, staleCandidateRejected,
      completionGenerationAdvance: (reopened.persistedCheckpoint.generation - beforeClosing.generation).toString(),
    }
  } finally {
    raw.close()
    repository.close()
  }
}

async function probeClosingRollback(input: Readonly<{
  engine: DirectZipEpochWriterV1
  journal: BrowserDirectZipJournal
  target: StagedDirectZipTarget
  admission: Parameters<DirectZipEpochWriterV1['beginFile']>[0]
  source: Parameters<DirectZipEpochWriterV1['resumeFile']>[0]
  rootComponent: string
  rollbackFault: 'before-publish' | 'unknown-tail'
}>) {
  const { engine, journal, target, admission, source, rootComponent, rollbackFault } = input
  const predecessor = journal.checkpoint
  let archiveOffset = admission.plan.zipEntry.localHeaderOffset + admission.plan.entryStreamBytes
  // More than the page-state cache limit must not evict the durable member's
  // rollback while later members replace the working rollback authority.
  for (let index = 0; index < 4; index++) {
    const plan = planDirectZipEntryV2({ ordinal: BigInt(index + 2), localHeaderOffset: archiveOffset,
      entry: { kind: 'file', path: [rootComponent, `pending-${index}.txt`], exactSize: source.exactSize } })
    const member = await engine.beginFile({ ...admission, plan, source: { ...source, fileId: identity(16, 30 + index) } })
    await member.write(Uint8Array.of(65, 66, 67, 68, 69, 70))
    await member.close()
    archiveOffset += plan.entryStreamBytes
  }
  const observe = target.observeCandidate.bind(target)
  let appendUnknownTail = rollbackFault === 'unknown-tail'
  target.observeCandidate = async (candidate, closeAttempt) => {
    if (appendUnknownTail && candidate.kind === 'closing') {
      appendUnknownTail = false
      target.appendExternal(Uint8Array.of(0xaa, 0xbb))
    }
    return observe(candidate, closeAttempt)
  }
  target.closeFaults.push('before-publish')
  const pages = await journal.pages.snapshot()
  let rejected = false
  try {
    await engine.closeArchive({ entryCount: pages.layoutRecordCount, centralDirectoryBytes: pages.centralBytes,
      layoutRoot: pages.layoutRoot, centralRoot: pages.centralRoot, predecessorEpochRoot: predecessor.epochRoot })
  } catch (error) {
    if (!(error instanceof Error) || !error.message.includes('closing epoch did not publish')) throw error
    rejected = true
  }
  if (!rejected || journal.pendingCandidate !== undefined || journal.checkpoint.member?.payloadOffset !== 3n) {
    throw new Error('failed final close did not retain the inside-member predecessor')
  }
  const resumed = engine.resumeFile(source)
  if (!('write' in resumed)) throw new Error('failed final close lost its source resume')
  await resumed.write(Uint8Array.of(68))
  const saved = await engine.pause()
  if (saved.kind !== 'advanced' || saved.checkpoint.member?.payloadOffset !== 4n) {
    throw new Error('same-journal resume lost committed member rollback authority')
  }
  await resumed.write(Uint8Array.of(69, 70))
  await resumed.close()
  return { sameJournalSavedOffset: saved.checkpoint.member.payloadOffset.toString(),
    truncated: target.truncateCount === 1, candidateRetired: journal.pendingCandidate === undefined }
}
