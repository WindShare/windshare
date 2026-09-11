import { localDeliveryFixture } from '../output/browser-delivery-local-fixture'
import { IndexedDbBrowserDeliveryRepository, advanceBrowserDeliveryRecord } from '../../src/output/browser-delivery'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb/receive-operation-repository'
import { IndexedDbReceiveResumeSource } from '../../src/output/browser/indexeddb-resume-state'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { createReceiveOperationV2, storedReceiveOperationRecord } from '../../src/output/workspace/records'
import { storedReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { deriveArtifactChoiceIdentity } from '../../src/transfer/intent'
import { beginBrowserDeliveryLocalMutation, reconcileBrowserDeliveryLifecycle } from '../../src/output/browser-delivery/recovery/local-lifecycle'
import { readBrowserDeliveryResumeSummary } from '../../src/output/browser-delivery/retained-authority'
import { INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, openIndexedDbCheckpointDatabase,
  requestResult, transactionCompletion } from '../../src/output/browser/indexeddb-database'
import { storedCheckpoint } from '../../src/output/browser/indexeddb/repository-transactions'
import type { FileCheckpointV2 } from '../../src/output/persistence/checkpoint'

export async function prepareLocalLifecycleCrash(databaseName: string) {
  const f = await localDeliveryFixture()
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  try {
    const choice = await deriveArtifactChoiceIdentity(f.intent.artifact, f.intent.plan)
    const operation = await createReceiveOperationV2({ receiveIntent: f.intent, preClickRanking: [choice.id] })
    await repository.commitTransition({ operationId: f.intent.operationId,
      records: [storedReceiveOperationRecord(operation), await storedReceiveLifecycleState(f.lifecycle)] })
    await journal.installPolicy(f.policy)
    await journal.createFile(f.initial)
    await persist(databaseName, f.baseline)
    await persist(databaseName, f.stage)
    await journal.replaceFile(f.initial, advanceBrowserDeliveryRecord(f.policy, f.initial, { kind: 'staged-complete', stage: f.stage }))
    const lease = await acquireBrowserReceiveOperationLease(repository, f.intent.operationId)
    try {
      await beginBrowserDeliveryLocalMutation({ repository, lease, databaseName })
      await persist(databaseName, f.target(3n, 2n))
      // The writer is gone but the lifecycle update never ran.
    } finally { await lease.release() }
    return (await journal.readFile(f.intent.operationId, f.source.fileId))!.localMutation?.lifecycleGeneration.toString()
  } finally { journal.close(); repository.close() }
}

export async function reopenLocalLifecycleCrash(databaseName: string) {
  const f = await localDeliveryFixture()
  const source = await IndexedDbReceiveResumeSource.open(databaseName)
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    const [partial] = await source.listLifecycleStates()
    if (partial?.kind !== 'resumable-receive' || partial.payloadKind !== 'file-set') throw new Error('Missing lifecycle')
    const summary = await source.readRecoverySummary(partial)
    const retained = await readBrowserDeliveryResumeSummary({ repository: journal, operationId: f.intent.operationId, databaseName })
    const lease = await acquireBrowserReceiveOperationLease(repository, f.intent.operationId)
    let busySummaryAbsent = false
    let unchangedGeneration = ''
    try {
      await beginBrowserDeliveryLocalMutation({ repository, lease, databaseName })
      unchangedGeneration = (await reconcileBrowserDeliveryLifecycle({ repository, lease, databaseName })).generation.toString()
      await persist(databaseName, f.target(8n, 3n))
      // Even while the writer owns the lease, only authenticated local mutation can suppress stale generic costs.
      const [busy] = await source.listLifecycleStates()
      if (busy?.kind !== 'resumable-receive' || busy.payloadKind !== 'file-set') throw new Error('Missing active lifecycle')
      busySummaryAbsent = await source.readRecoverySummary(busy) === undefined
    } finally { await lease.release() }
    return {
      partialGeneration: partial.generation.toString(), partialBytes: summary?.verifiedPartialBytes.toString(),
      retainedAction: retained?.localContinuation, unchangedGeneration, busySummaryAbsent,
    }
  } finally { repository.close(); journal.close(); source.close() }
}

export async function finishLocalLifecycleCrash(databaseName: string) {
  const source = await IndexedDbReceiveResumeSource.open(databaseName)
  try {
    const [saved] = await source.listLifecycleStates()
    if (saved?.kind !== 'resumable-receive' || saved.payloadKind !== 'file-set') throw new Error('Missing saved lifecycle')
    const summary = await source.readRecoverySummary(saved)
    const [again] = await source.listLifecycleStates()
    return { generation: saved.generation.toString(), completedBytes: summary?.completedBytes.toString(),
      completedFiles: summary?.completedFileCount.toString(), idempotent: again?.generation === saved.generation }
  } finally { source.close() }
}

async function persist(databaseName: string, checkpoint: FileCheckpointV2) {
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const transaction = database.transaction(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, 'readwrite')
    const completion = transactionCompletion(transaction)
    await requestResult(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).put(storedCheckpoint(checkpoint)))
    await completion
  } finally { database.close() }
}
