import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  INDEXEDDB_DIRECT_ZIP_STATE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE, INDEXEDDB_RECEIVE_RECORD_STORE, INDEXEDDB_RECEIVE_HANDLE_STORE,
  openIndexedDbCheckpointDatabase, requestResult, transactionCompletion,
} from '../../../src/output/browser/indexeddb-database'
import {
  IndexedDbDirectZipJournalRepository, createDirectZipCheckpointV1, createDirectZipCheckpointProposalV1,
  createDirectZipImmutablePageV1, createDirectZipRollbackCandidateV1, createDirectZipStateRowV1,
  createDirectZipTargetObservationV1,
  type DirectZipImmutablePageV1, type DirectZipPageChainV1,
} from '../../../src/output/direct-zip/journal'
import { directZipPageStore } from '../../../src/output/direct-zip/journal/indexeddb/authority'
import { initialReceiveLifecycleState, nextReceiveLifecycleState } from '../../../src/output/workspace/state'
import { storedReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import { receiveOperationHandleRecord, receiveOperationLeaseRecord } from '../../../src/output/workspace/records'
import { rollbackCheckpointFixture } from '../../output/direct-zip/journal/rollback-fixture'

export async function probeDirectZipRollbackFences(databaseName: string) {
  let repository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
  const raw = await openIndexedDbCheckpointDatabase(databaseName)
  const fixture = await rollbackFixture()
  const { checkpoint, pages, proposal } = fixture
  const operationId = checkpoint.operationId
  const oldLeaseId = identity(16, 71)
  const newLeaseId = identity(16, 72)
  const oldFence = { operationId, leaseId: oldLeaseId, checkpointGeneration: checkpoint.generation }
  const newFence = { ...oldFence, leaseId: newLeaseId }
  const receiving = nextReceiveLifecycleState(initialReceiveLifecycleState({
    operationId, receiveIntentDigest: checkpoint.receiveIntentDigest,
  }), { kind: 'receiving', activeLeaseId: oldLeaseId })
  const handles = ['parent', 'file'].map((id, ordinal) => receiveOperationHandleRecord({
    id, operationId, kind: ordinal + 1, authorityRef: identity(32, ordinal + 1), handle: { id },
  }))
  const candidate = await createDirectZipRollbackCandidateV1({
    operationId, candidateId: identity(16, 73), leaseId: oldLeaseId,
    predecessorCheckpointGeneration: checkpoint.generation, predecessorCheckpointDigest: checkpoint.digest,
    predecessorTargetObservation: checkpoint.targetObservation, proposedCheckpoint: proposal,
  })
  try {
    await putRows(raw, [
      ...pages.map(page => [directZipPageStore(page.pageKind), page] as const),
      [INDEXEDDB_DIRECT_ZIP_STATE_STORE, await createDirectZipStateRowV1(checkpoint, oldLeaseId)],
      [INDEXEDDB_RECEIVE_LEASE_STORE, receiveOperationLeaseRecord({ operationId, leaseId: oldLeaseId, acquiredAt: 1 })],
      [INDEXEDDB_RECEIVE_RECORD_STORE, await storedReceiveLifecycleState(receiving)],
      ...handles.map(handle => [INDEXEDDB_RECEIVE_HANDLE_STORE, handle] as const),
    ])
    await repository.bindRollbackCandidate(oldFence, candidate)
    repository.close()
    repository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
    const persistedIntent = await repository.readOperationCandidate(operationId)
    // Lease takeover is a separate existing protocol; seed its atomic result to isolate rollback CAS.
    await putRows(raw, [
      [INDEXEDDB_DIRECT_ZIP_STATE_STORE, await createDirectZipStateRowV1(checkpoint, newLeaseId)],
      [INDEXEDDB_RECEIVE_LEASE_STORE, receiveOperationLeaseRecord({ operationId, leaseId: newLeaseId, acquiredAt: 2 })],
    ])
    const targetObservation = await createDirectZipTargetObservationV1({
      ...checkpoint.targetObservation, exactLength: proposal.archiveOffset,
      epochRootDigest: proposal.epochRootDigest, lastModifiedMilliseconds: 2,
    })
    const restored = await createDirectZipCheckpointV1({
      ...proposal, candidateLineageDigest: candidate.digest, targetObservation,
    })
    const resumable = nextReceiveLifecycleState(receiving, {
      kind: 'resumable-receive', payloadKind: 'direct-zip', directZipCheckpointDigest: restored.digest,
      safeSelectedPayloadBytes: restored.committedSelectedPayloadBytes,
      committedArchiveLength: restored.committedArchiveLength, checkpointPhase: restored.phase,
    })
    const cut = {
      fence: newFence, candidate, checkpoint: restored,
      lifecycle: resumable, lifecycleRecord: await storedReceiveLifecycleState(resumable),
    }
    const staleLeaseFailure = await failure(() => repository.promoteRollbackCandidate({ ...cut, fence: oldFence }))
    const forgedObservation = await createDirectZipTargetObservationV1({
      ...targetObservation, ownershipMarkerDigest: identity(32, 74),
    })
    const forged = await createDirectZipCheckpointV1({ ...restored, targetObservation: forgedObservation })
    const forgedObservationFailure = await failure(() => repository.promoteRollbackCandidate({ ...cut, checkpoint: forged }))
    const lateFault = await failure(() => repository.promoteRollbackCandidate({
      ...cut, handles: [receiveOperationHandleRecord({
        ...handles[0]!, handle: { uncloneable: () => undefined },
      })],
    }))
    const unchangedAfterFailures = (await repository.readState(operationId))?.checkpointDigest === checkpoint.digest &&
      (await repository.readOperationCandidate(operationId))?.digest === candidate.digest
    await repository.promoteRollbackCandidate(cut)
    const finalState = await repository.readState(operationId)
    const staleGenerationFailure = await failure(() => repository.promoteRollbackCandidate(cut))
    const orphanCollection = await repository.collectOrphanPages({
      ...newFence, checkpointGeneration: restored.generation,
    })
    const handleTransaction = raw.transaction(INDEXEDDB_RECEIVE_HANDLE_STORE, 'readonly')
    const savedHandles = await Promise.all(handles.map(handle => requestResult(
      handleTransaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE).get(handle.id),
    )))
    await transactionCompletion(handleTransaction)
    return {
      persistedIntent: persistedIntent?.digest === candidate.digest,
      creationLeasePreserved: persistedIntent?.leaseId === oldLeaseId,
      staleLeaseFailure, forgedObservationFailure, lateFault, unchangedAfterFailures,
      currentLeasePromoted: finalState?.leaseId === newLeaseId && finalState.checkpointDigest === restored.digest,
      restoredArchiveLength: finalState?.checkpoint.committedArchiveLength.toString(),
      candidateAbsent: (await repository.readOperationCandidate(operationId)) === undefined,
      staleGenerationFailure, deletedSuffixPages: orphanCollection.deletedPageCount.toString(),
      handlesPreserved: savedHandles.every((handle, index) => handle.id === handles[index]!.id),
    }
  } finally {
    repository.close()
    raw.close()
    await new Promise<void>((resolve, reject) => {
      const request = indexedDB.deleteDatabase(databaseName)
      request.onsuccess = () => resolve()
      request.onerror = () => reject(request.error)
    })
  }
}

async function rollbackFixture() {
  const base = await rollbackCheckpointFixture()
  const pages: DirectZipImmutablePageV1[] = []
  const chains: Partial<Record<DirectZipImmutablePageV1['pageKind'], DirectZipPageChainV1>> = {}
  const append = async (kind: DirectZipImmutablePageV1['pageKind'], id: number) => {
    const previous = pages.at(-1)
    const chain = chains[kind]
    const page = await createDirectZipImmutablePageV1({
      operationId: base.operationId, pageKind: kind, chainId: identity(16, id),
      pageOrdinal: Number(chain?.pageCount ?? 0n), predecessorRootDigest: chain?.rootDigest ?? identity(32, 0),
      canonicalEntries: [Uint8Array.of(id)], previousBudgetUsage: previous?.budgetUsage ?? { memberCount: 0n, canonicalMetadataBytes: 0n },
      previousChainRecordCount: chain?.recordCount ?? 0n,
      previousChainCanonicalMetadataBytes: chain?.canonicalMetadataBytes ?? 0n,
      accountingPredecessor: previous === undefined
        ? { kind: 'checkpoint', checkpointGeneration: 1n, checkpointDigest: identity(32, 81) }
        : { kind: 'page', pageId: previous.id, pageKind: previous.pageKind, pageDigest: previous.digest },
    })
    pages.push(page)
    chains[kind] = pageChain(page)
    return page
  }
  await append('layout', 82)
  await append('central', 83)
  const prefixTail = await append('epoch', 84)
  const rollback = {
    ...base.currentMember!.rollback, layoutPages: chains.layout!, centralPages: chains.central!, epochPages: chains.epoch!,
    journalUsage: prefixTail.budgetUsage, accountingTailPageId: prefixTail.id,
  }
  await append('layout', 82)
  const currentTail = await append('epoch', 84)
  const checkpoint = await createDirectZipCheckpointV1({
    ...base, currentMember: { ...base.currentMember!, rollback },
    layoutPages: chains.layout!, centralPages: chains.central!, epochPages: chains.epoch!,
    journalUsage: currentTail.budgetUsage, accountingTailPageId: currentTail.id,
  })
  const { currentMember, ...rest } = checkpoint
  if (currentMember === undefined) throw new TypeError('fixture lost its active member')
  const proposal = await createDirectZipCheckpointProposalV1({
    ...rest, generation: checkpoint.generation + 1n, predecessorCheckpointDigest: checkpoint.digest,
    phase: 'between-members', archiveOffset: rollback.archiveOffset, committedArchiveLength: rollback.archiveOffset,
    committedSelectedPayloadBytes: rollback.safeSelectedPayloadBytes, epochRootDigest: rollback.epochRootDigest,
    layoutPages: rollback.layoutPages, centralPages: rollback.centralPages, epochPages: rollback.epochPages,
    journalUsage: rollback.journalUsage, accountingTailPageId: rollback.accountingTailPageId,
  })
  return { checkpoint, proposal, pages }
}

function pageChain(page: DirectZipImmutablePageV1): DirectZipPageChainV1 {
  return {
    chainId: page.chainId, rootDigest: page.chainRootDigest, pageCount: BigInt(page.pageOrdinal + 1),
    recordCount: page.chainRecordCount, canonicalMetadataBytes: page.chainCanonicalMetadataBytes,
  }
}

async function putRows(database: IDBDatabase, rows: readonly (readonly [string, unknown])[]) {
  const transaction = database.transaction([...new Set(rows.map(([store]) => store))], 'readwrite')
  for (const [store, value] of rows) transaction.objectStore(store).put(value)
  await transactionCompletion(transaction)
}

async function failure(action: () => Promise<void>) {
  try { await action(); return 'none' } catch (error) { return error instanceof Error ? error.name : 'Error' }
}

function identity(width: number, fill: number): string { return encodeBase64Url(new Uint8Array(width).fill(fill)) }
