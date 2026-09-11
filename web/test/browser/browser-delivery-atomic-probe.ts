import { openBrowserFolderDelivery } from '../../src/output/browser-delivery/assembly'
import { IndexedDbBrowserDeliveryRepository } from '../../src/output/browser-delivery/indexeddb'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_FINAL_PROOF_STORE, openIndexedDbCheckpointDatabase, requestResult } from '../../src/output/browser/indexeddb-database'
import { reopenFileSystemAccessOutput, type FileSystemAccessOutputSession } from '../../src/output/file-system-access/session'
import type { ReceiveIntent } from '../../src/transfer/intent'
import { bindTask, resultRootArtifact, type FsaNamespaceFixture } from './fsa-namespace-atomicity-harness'
import { deliveryIdentity } from '../output/browser-delivery-fixture'

const FILE_ID = deliveryIdentity(90, 16)
const SOURCE_REVISION = deliveryIdentity(91, 16)
const FILE_PATH = ['atomic.bin']
const BYTES = Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8)
const request = () => ({
  sourceAuthenticationPath: ['shared', ...FILE_PATH], materializationRelativePath: FILE_PATH,
  recovery: { pausedFile: 'preserve' as const },
  openRevision: async () => ({ fileId: FILE_ID, fileRevision: SOURCE_REVISION, exactSize: BigInt(BYTES.length) }),
})

async function openDelivery(target: FileSystemAccessOutputSession, fixture: FsaNamespaceFixture, preference: 'direct' | 'automatic') {
  return openBrowserFolderDelivery({
    target, intent: target.intent, preference, storage: navigator.storage,
    storageFacts: async () => ({ opfs: 'usable', pressure: 'normal', persistence: 'not-persisted', quota: { kind: 'unknown' } }),
    databaseName: fixture.databaseName, capacityDatabaseName: fixture.databaseName + '-capacity',
    operationLease: { operationId: target.intent.operationId, leaseId: 'atomic-test-lease' },
  })
}

export async function prepareAtomicDirect(fixture: FsaNamespaceFixture, preference: 'direct' | 'automatic') {
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName, { create: true })
  const operations = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const target = await bindTask(fixture, parent, operations, await resultRootArtifact(), 70)
  const delivery = await openDelivery(target, fixture, preference)
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName: fixture.databaseName })
  const database = await openIndexedDbCheckpointDatabase(fixture.databaseName)
  const originalAdd = IDBObjectStore.prototype.add
  try {
    IDBObjectStore.prototype.add = function(...args: Parameters<IDBObjectStore['add']>) {
      const result = originalAdd.apply(this, args)
      if (this.transaction.db.name === fixture.databaseName && this.name === INDEXEDDB_BROWSER_DELIVERY_FILE_STORE) this.transaction.abort()
      return result
    }
    const initialAborted = await rejects(() => delivery.beginFile(request()))
    IDBObjectStore.prototype.add = originalAdd
    const recordAfterAbort = await journal.readFile(target.intent.operationId, FILE_ID)
    const candidateCount = await requestResult(database.transaction(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
      .objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE).count())
    const speculativeFiles = delivery.getSummary().receivingFiles
    const transaction = await delivery.beginFile(request())
    const initialState = (await journal.readFile(target.intent.operationId, FILE_ID))?.state.kind
    await transaction.writeRange(0n, BYTES.subarray(0, 3))
    await transaction.checkpoint()
    await transaction.pause()
    const samePageResume = await delivery.beginFile(request())
    if (samePageResume.initialDurableRanges[0]?.end !== 3n) throw new Error('Paused direct writer lost its commit enrollment or durable prefix')
    await samePageResume.pause()
    const retainedBytes = (await target.readCheckpoint(FILE_ID))!.verifiedRanges[0]!.end.toString()
    return { fixture, preference, intent: target.intent, initialAborted,
      absentAfterAbort: recordAfterAbort === undefined, candidateCount, speculativeFiles, initialState, retainedBytes }
  } finally {
    IDBObjectStore.prototype.add = originalAdd
    await delivery.close()
    database.close(); journal.close(); operations.close()
  }
}

export async function finishAtomicDirect(input: {
  fixture: FsaNamespaceFixture; preference: 'direct' | 'automatic'; intent: ReceiveIntent
}, failure: 'delivery' | 'target-proof') {
  const { fixture } = input
  const operations = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const target = await reopenFileSystemAccessOutput({
    intent: input.intent, operationRepository: operations, databaseName: fixture.databaseName,
  })
  const delivery = await openDelivery(target, fixture, input.preference)
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName: fixture.databaseName })
  const database = await openIndexedDbCheckpointDatabase(fixture.databaseName)
  const originalPut = IDBObjectStore.prototype.put
  const originalAdd = IDBObjectStore.prototype.add
  const originalTransaction = IDBDatabase.prototype.transaction
  try {
    const transaction = await delivery.beginFile(request())
    const resumedBytes = transaction.initialDurableRanges[0]!.end.toString()
    await transaction.writeRange(3n, BYTES.subarray(3))
    IDBObjectStore.prototype.put = function(...args: Parameters<IDBObjectStore['put']>) {
      const result = originalPut.apply(this, args)
      if (failure === 'delivery' && this.transaction.db.name === fixture.databaseName &&
          this.name === INDEXEDDB_BROWSER_DELIVERY_FILE_STORE) this.transaction.abort()
      return result
    }
    IDBObjectStore.prototype.add = function(...args: Parameters<IDBObjectStore['add']>) {
      const result = originalAdd.apply(this, args)
      if (failure === 'target-proof' && this.transaction.db.name === fixture.databaseName &&
          this.name === INDEXEDDB_FILE_FINAL_PROOF_STORE) this.transaction.abort()
      return result
    }
    const finalAborted = await rejects(() => transaction.commit())
    IDBObjectStore.prototype.put = originalPut
    IDBObjectStore.prototype.add = originalAdd
    const stateAfterAbort = (await journal.readFile(input.intent.operationId, FILE_ID))?.state.kind
    const retainedAfterAbort = (await target.readCheckpoint(FILE_ID))!.verifiedRanges[0]!.end.toString()
    const savedAfterAbort = delivery.getSummary().targetSavedBytes.toString()
    const proofCountAfterAbort = await requestResult(database.transaction(INDEXEDDB_FILE_FINAL_PROOF_STORE)
      .objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE).count())
    let finalTransactions = 0
    IDBDatabase.prototype.transaction = function(...args: Parameters<IDBDatabase['transaction']>) {
      if (this.name === fixture.databaseName) finalTransactions++
      return originalTransaction.apply(this, args)
    }
    await transaction.commit()
    IDBDatabase.prototype.transaction = originalTransaction
    const finalState = (await journal.readFile(input.intent.operationId, FILE_ID))?.state.kind
    const finalBytes = delivery.getSummary().targetSavedBytes.toString()
    const payload = await (await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName))
      .getDirectoryHandle(target.reservation.physicalName)
    const actual = new Uint8Array(await (await (await payload.getFileHandle(FILE_PATH[0]!)).getFile()).arrayBuffer())
    return { resumedBytes, finalAborted, stateAfterAbort, retainedAfterAbort, savedAfterAbort,
      proofCountAfterAbort, finalTransactions, finalState, finalBytes, contentsMatch: actual.every((byte, index) => byte === BYTES[index]) && actual.length === BYTES.length }
  } finally {
    IDBObjectStore.prototype.put = originalPut
    IDBObjectStore.prototype.add = originalAdd
    IDBDatabase.prototype.transaction = originalTransaction
    await delivery.close()
    database.close(); journal.close(); operations.close()
  }
}

async function rejects(action: () => Promise<unknown>): Promise<boolean> {
  try { await action(); return false } catch { return true }
}
