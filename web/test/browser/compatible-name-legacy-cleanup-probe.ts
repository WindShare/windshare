import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  INDEXEDDB_V10_STORE_SCHEMAS,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from '../../src/output/browser/indexeddb-database'
import { forgetLegacyCompatibleNameRecord } from '../../src/output/browser/indexeddb/compatible-name-legacy-cleanup'
import { IndexedDbReceiveResumeSource } from '../../src/output/browser/indexeddb-resume-state'
import { browserReceiveOperationLockName } from '../../src/output/browser/session-lease'
import { ReceiveOperationResumeAuthority } from '../../src/output/resume/authority'
import { initialReceiveLifecycleState } from '../../src/output/workspace/state'
import { storedReceiveLifecycleState } from '../../src/output/workspace/state-codec'

export async function probeLegacyCompatibleNameCleanup(databaseName: string) {
  const operationId = identity(16, 41)
  const foreignId = identity(16, 42)
  const lifecycle = {
    ...initialReceiveLifecycleState({ operationId, receiveIntentDigest: identity(32, 43) }),
    kind: 'receiving' as const,
    activeLeaseId: identity(16, 44),
  }
  const lifecycleRow = await storedReceiveLifecycleState(lifecycle)
  const directory = await navigator.storage.getDirectory()
  const fileName = databaseName + '.sentinel'
  const file = await directory.getFileHandle(fileName, { create: true })
  const writable = await file.createWritable()
  await writable.write('previously downloaded content')
  await writable.close()
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const transaction = database.transaction(INDEXEDDB_V10_STORE_SCHEMAS.map(schema => schema.name), 'readwrite')
    const complete = transactionCompletion(transaction)
    for (const schema of INDEXEDDB_V10_STORE_SCHEMAS) {
      const store = transaction.objectStore(schema.name)
      store.put({ [schema.keyPath]: foreignId, operationId: foreignId })
      store.put({ [schema.keyPath]: operationId, operationId })
    }
    transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE).put({
      operationId,
      formatVersion: 'compatible-name-ledger/v2',
      pair: { script: { physicalName: 'restore-old.ps1' }, sidecar: { physicalName: 'independent.data' } },
    })
    transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).put(lifecycleRow)
    await complete

    const source = await IndexedDbReceiveResumeSource.open(databaseName)
    const authority = new ReceiveOperationResumeAuthority({
      source,
      mutations: {
        resume: async () => { throw new Error('must not resume') },
        expire: async () => { throw new Error('must not expire physically') },
        discard: descriptor => forgetLegacyCompatibleNameRecord(descriptor, { databaseName }),
      },
    })
    const inventory = await authority.listResumeState()
    const reference = inventory.operations[0]!
    const continuation = reference.descriptor.continuation
    const stale = { ...reference.descriptor, lifecycle: { ...lifecycle, generation: lifecycle.generation + 1n } }
    const staleRejected = await forgetLegacyCompatibleNameRecord(stale, { databaseName })
      .then(() => false, () => true)
    const busyRejected = await navigator.locks.request(browserReceiveOperationLockName(operationId),
      () => forgetLegacyCompatibleNameRecord(reference.descriptor, { databaseName })
        .then(() => false, () => true))
    const result = await authority.discard(reference)
    inventory.close()
    const remainingOperations = (await source.listLifecycleStates()).length
    source.close()
    const countsTransaction = database.transaction(INDEXEDDB_V10_STORE_SCHEMAS.map(schema => schema.name), 'readonly')
    const countsComplete = transactionCompletion(countsTransaction)
    const remainingCounts = await Promise.all(INDEXEDDB_V10_STORE_SCHEMAS.map(schema =>
      requestResult(countsTransaction.objectStore(schema.name).count())))
    await countsComplete
    return {
      continuation, staleRejected, busyRejected, result, remainingOperations,
      onlyForeignRowsRemain: remainingCounts.every(count => count === 1),
      fileContent: await (await file.getFile()).text(),
    }
  } finally {
    database.close()
    await requestResult(indexedDB.deleteDatabase(databaseName))
    await directory.removeEntry(fileName)
  }
}

function identity(length: number, byte: number): string {
  return encodeBase64Url(new Uint8Array(length).fill(byte))
}
