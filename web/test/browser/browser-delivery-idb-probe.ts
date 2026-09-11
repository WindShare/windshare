import {
  IndexedDbBrowserDeliveryRepository, createBrowserDeliveryRecord, snapshotBrowserDeliveryRecord, authorizeBrowserDeliveryRestart,
  createBrowserSavePolicy, summarizeBrowserDeliveries, readBrowserDeliveryResumeSummary,
} from '../../src/output/browser-delivery'
import {
  INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_FILE_FINAL_PROOF_STORE,
  openIndexedDbCheckpointDatabase, requestResult, transactionCompletion,
} from '../../src/output/browser/indexeddb-database'
import { storedCheckpoint } from '../../src/output/browser/indexeddb/repository-transactions'
import { finalFileCheckpointProof } from '../../src/output/persistence/journal'
import { newFileCheckpointV2, type FileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import { createMaterializationLedgerBinding } from '../../src/output/materialization-ledger/codec'
import { createFinalizedFileMaterializationRecords } from '../../src/output/materialization-ledger/journal'
import { VerifiedFinalOutputFile } from '../../src/transfer/output-session'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import { deliveryFixture, deliveryIdentity } from '../output/browser-delivery-fixture'

export async function restartDeliveryJournal(databaseName: string) {
  const f = deliveryFixture('direct')
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  try {
    await journal.installPolicy(f.policy)
    const forged = snapshotBrowserDeliveryRecord(f.policy, { ...f.initial, localMutation: {
      lifecycleGeneration: 1n, checkpointSetDigest: deliveryIdentity(90),
    } })
    const forgedMutationRejected = await rejects(() => journal.createFile(forged))
    await journal.createFile(f.initial)
    const partial = f.checkpoint('direct', 3n)
    await persistCheckpoint(databaseName, partial)
    const receiving = f.advance(f.initial, { kind: 'receiving', checkpoint: partial })
    await journal.replaceFile(f.initial, receiving)
    const authorized = authorizeBrowserDeliveryRestart(f.policy, receiving, partial, 'explicit-redownload')
    const ordinaryResetRejected = await rejects(() => journal.replaceFile(receiving, authorized))
    const marker = await journal.authorizeRestart(receiving, partial, 'explicit-redownload')
    const reset = newFileCheckpointV2({ ...partial, checkpointGeneration: 2n, stateGeneration: 2n, verifiedRanges: [] })
    await persistCheckpoint(databaseName, reset)
    const reopened = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
    try {
      const retained = (await reopened.readFile(f.policy.operationId, f.source.fileId))!
      const next = f.advance(retained, { kind: 'receiving', checkpoint: reset })
      await reopened.replaceFile(retained, next)
      const resumed = newFileCheckpointV2({ ...reset, checkpointGeneration: 3n, stateGeneration: 3n,
        verifiedRanges: [{ start: 0n, end: 2n }] })
      await persistCheckpoint(databaseName, resumed)
      await reopened.replaceFile(next, f.advance(next, { kind: 'receiving', checkpoint: resumed }))
      return { forgedMutationRejected, ordinaryResetRejected, markerState: marker.state.kind,
        reopenedState: (await reopened.readFile(f.policy.operationId, f.source.fileId))!.state.kind }
    } finally { reopened.close() }
  } finally { journal.close() }
}

export async function prepareBrowserDeliveryReopen(databaseName: string) {
  const f = deliveryFixture()
  const repository = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  try {
    await repository.installPolicy(f.policy)
    await repository.createFile(f.initial)
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    const missingStageRejected = await rejects(() => repository.replaceFile(f.initial, complete))
    await persistCheckpoint(databaseName, f.stage!)
    const crashGapSummary = await readBrowserDeliveryResumeSummary({ repository, operationId: f.policy.operationId, databaseName })
    await repository.replaceFile(f.initial, complete)
    const copying = f.advance(complete, {
      kind: 'copying', stage: f.stage!, attempt: { attemptId: 'crashed-copy', targetOwnedObjectId: f.target.ownedObjectId },
    })
    await repository.replaceFile(complete, copying)
    const saved = f.advance(copying, { kind: 'target-saved', stage: f.stage!, target: f.target })
    const missingTargetRejected = await rejects(() => repository.replaceFile(copying, saved))
    return {
      missingStageRejected, missingTargetRejected,
      crashGapContinuation: crashGapSummary?.localContinuation, crashGapTargetBytes: crashGapSummary?.targetSavedBytes.toString(),
      retainedState: (await repository.readFile(f.policy.operationId, f.source.fileId))!.state.kind,
    }
  } finally { repository.close() }
}

export async function finishBrowserDeliveryReopen(databaseName: string) {
  const f = deliveryFixture()
  const repository = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  try {
    const policy = (await repository.readPolicy(f.policy.operationId))!
    const copying = (await repository.readFile(f.policy.operationId, f.source.fileId))!
    const before = summarizeBrowserDeliveries(policy, [copying])
    // Simulates target-authority reconciliation after the old writer has drained.
    const retry = f.advance(copying, { kind: 'staged-complete', stage: f.stage!, failureReason: 'interrupted target copy drained' })
    await repository.replaceFile(copying, retry)
    const nextCopy = f.advance(retry, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'retry-copy' } })
    await repository.replaceFile(retry, nextCopy)
    await persistCheckpoint(databaseName, f.target)
    const checkpointOnlyRejected = await rejects(() => repository.replaceFile(nextCopy,
      f.advance(nextCopy, { kind: 'target-saved', stage: f.stage!, target: f.target })))
    await persistFinalProof(databaseName, f.target)
    const saved = f.advance(nextCopy, { kind: 'target-saved', stage: f.stage!, target: f.target })
    await repository.replaceFile(nextCopy, saved)
    const pending = f.advance(saved, { kind: 'cleanup-pending', stage: f.stage!, target: f.target })
    await repository.replaceFile(saved, pending)
    // A crash after physical deletion must still allow the already-authorized cleanup completion.
    await deleteCheckpoint(databaseName, f.stage!.recordId)
    const cleaned = f.advance(pending, { kind: 'cleaned', target: f.target })
    await repository.replaceFile(pending, cleaned)
    const after = summarizeBrowserDeliveries(policy, [(await repository.readFile(policy.operationId, f.source.fileId))!])
    return {
      checkpointOnlyRejected,
      beforeContinuation: before.localContinuation, beforeTargetSavedBytes: before.targetSavedBytes.toString(),
      beforeStagedBytes: before.stagedBytes.toString(), afterTargetSavedBytes: after.targetSavedBytes.toString(),
      afterStagedBytes: after.stagedBytes.toString(), afterState: cleaned.state.kind,
    }
  } finally { repository.close() }
}

export async function raceBrowserDeliveryJournal(databaseName: string) {
  const f = deliveryFixture('direct')
  const first = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  const second = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  try {
    await first.installPolicy(f.policy)
    const conflict = createBrowserSavePolicy({ ...f.policy, preference: 'automatic' })
    const policyConflictRejected = await rejects(() => second.installPolicy(conflict))
    await Promise.all([first.createFile(f.initial), second.createFile(f.initial)])
    await persistCheckpoint(databaseName, f.target)
    await persistFinalProof(databaseName, f.target)
    const saved = f.advance(f.initial, { kind: 'target-saved', target: f.target })
    const race = await Promise.allSettled([
      first.replaceFile(f.initial, saved), second.replaceFile(f.initial, saved),
    ])
    const after = await second.createFile(f.initial)
    const changed = createBrowserDeliveryRecord({
      policy: f.policy, source: { ...f.source, fileRevision: deliveryIdentity(51, 16) },
      materializationRelativePath: f.source.canonicalPath, placement: 'direct', placementReason: f.initial.placementReason,
    })
    const sourceConflictRejected = await rejects(() => first.createFile(changed))
    const entries = await Promise.all([21, 22, 23].map(byte => first.createFile(createBrowserDeliveryRecord({
      policy: f.policy, source: { ...f.source, fileId: deliveryIdentity(byte, 16), canonicalPath: [byte + '.bin'] },
      materializationRelativePath: [byte + '.bin'], placement: 'direct', placementReason: 'paging',
    }))))
    const pageOne = await first.scanFiles({ operationId: f.policy.operationId, limit: 2 })
    const pageTwo = await first.scanFiles({ operationId: f.policy.operationId, limit: 2, afterFileId: pageOne.nextFileId! })
    return {
      policyConflictRejected, sourceConflictRejected,
      winners: race.filter(result => result.status === 'fulfilled').length,
      losers: race.filter(result => result.status === 'rejected').length,
      retriedPlacementState: after.state.kind,
      pages: [pageOne.records.length, pageTwo.records.length],
      uniqueFiles: new Set([...pageOne.records, ...pageTwo.records].map(record => record.fileId)).size,
      expectedFiles: entries.length + 1,
    }
  } finally { first.close(); second.close() }
}

export async function finalizeDirectDelivery(databaseName: string) {
  const f = deliveryFixture('direct')
  const repository = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  const proof = finalFileCheckpointProof(f.target)
  const originalTransaction = IDBDatabase.prototype.transaction
  try {
    await repository.installPolicy(f.policy)
    await repository.createFile(f.initial)
    const missingCheckpointRejected = await rejects(() => repository.finalizeDirect(f.initial, proof))
    await persistCheckpoint(databaseName, f.target)
    const missingFinalProofRejected = await rejects(() => repository.finalizeDirect(f.initial, proof))
    await persistFinalProof(databaseName, f.target)
    const foreignProofRejected = await rejects(() => repository.finalizeDirect(f.initial, { ...proof, fileRevision: deliveryIdentity(60, 16) }))
    let finalizationTransactions = 0
    IDBDatabase.prototype.transaction = function(...args: Parameters<IDBDatabase['transaction']>) {
      if (this.name === databaseName) finalizationTransactions += 1
      return originalTransaction.apply(this, args)
    }
    const cleaned = await repository.finalizeDirect(f.initial, proof)
    IDBDatabase.prototype.transaction = originalTransaction
    const retry = await repository.finalizeDirect(f.initial, proof)
    return {
      missingCheckpointRejected, missingFinalProofRejected, foreignProofRejected, finalizationTransactions,
      state: cleaned.state.kind, generation: cleaned.generation.toString(), idempotent: retry.digest === cleaned.digest,
      targetBytes: summarizeBrowserDeliveries(f.policy, [cleaned]).targetSavedBytes.toString(),
    }
  } finally { IDBDatabase.prototype.transaction = originalTransaction; repository.close() }
}

export async function abortBrowserDeliveryCut(databaseName: string) {
  const f = deliveryFixture()
  const repository = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  const originalPut = IDBObjectStore.prototype.put
  try {
    await repository.installPolicy(f.policy)
    await repository.createFile(f.initial)
    await persistCheckpoint(databaseName, f.stage!)
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    IDBObjectStore.prototype.put = function(...args: Parameters<IDBObjectStore['put']>) {
      const result = originalPut.apply(this, args)
      if (this.name === INDEXEDDB_BROWSER_DELIVERY_FILE_STORE) this.transaction.abort()
      return result
    }
    const aborted = await rejects(() => repository.replaceFile(f.initial, complete))
    IDBObjectStore.prototype.put = originalPut
    const prior = (await repository.readFile(f.policy.operationId, f.source.fileId))!
    await repository.replaceFile(prior, complete)
    return { aborted, priorState: prior.state.kind, retryState: (await repository.readFile(f.policy.operationId, f.source.fileId))!.state.kind }
  } finally { IDBObjectStore.prototype.put = originalPut; repository.close() }
}

async function persistCheckpoint(databaseName: string, checkpoint: FileCheckpointV2): Promise<void> {
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const transaction = database.transaction(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, 'readwrite')
    const completed = transactionCompletion(transaction)
    await requestResult(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).put(storedCheckpoint(checkpoint)))
    await completed
  } finally { database.close() }
}

async function persistFinalProof(databaseName: string, checkpoint: FileCheckpointV2): Promise<void> {
  const binding = await createMaterializationLedgerBinding({
    operationId: checkpoint.operationId, receiveIntentDigest: checkpoint.receiveIntentDigest,
    materializationBindingDigest: checkpoint.materializationBindingDigest, authorityRef: checkpoint.authorityRef,
  })
  const records = await createFinalizedFileMaterializationRecords({
    binding, finalCheckpoint: checkpoint,
    finalOutput: new VerifiedFinalOutputFile({
      backend: 'browser-fsa', outputSessionId: 'journal-test-session',
      canonicalPath: snapshotMaterializationRootRelativePath(checkpoint.canonicalPath),
      ownedFileIdentity: checkpoint.ownedObjectId,
    }, { shareInstance: deliveryIdentity(55, 16), fileId: checkpoint.fileId, fileRevision: checkpoint.fileRevision }, checkpoint.exactSize),
  })
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const transaction = database.transaction(INDEXEDDB_FILE_FINAL_PROOF_STORE, 'readwrite')
    const completed = transactionCompletion(transaction)
    await requestResult(transaction.objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE).put(records.finalProof))
    await completed
  } finally { database.close() }
}

async function deleteCheckpoint(databaseName: string, recordId: string): Promise<void> {
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const transaction = database.transaction(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, 'readwrite')
    const completed = transactionCompletion(transaction)
    await requestResult(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).delete(recordId))
    await completed
  } finally { database.close() }
}

async function rejects(action: () => Promise<unknown>): Promise<boolean> {
  try { await action(); return false } catch { return true }
}
