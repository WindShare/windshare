import type { ReceiveOperationResumeDescriptor } from '../../resume/descriptor'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  operationRecordId,
} from '../../workspace/records'
import { storedReceiveLifecycleState } from '../../workspace/state-codec'
import {
  DEFAULT_OUTPUT_DATABASE_NAME,
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  INDEXEDDB_V10_STORE_SCHEMAS,
  isIndexedDbRecord,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from '../indexeddb-database'
import {
  BrowserReceiveOperationBusyError,
  browserReceiveOperationLockName,
  type BrowserLockManagerRuntime,
} from '../session-lease'

const LEGACY_LEDGER_FORMATS = new Set([
  'compatible-name-ledger/v1',
  'compatible-name-ledger/v2',
])

// Recognizing an obsolete envelope grants only metadata deletion; its paths,
// pair identities, and persisted handles never become filesystem authority.
export function isLegacyCompatibleNameRecord(value: unknown, operationId: string): boolean {
  return isIndexedDbRecord(value) && value.operationId === operationId &&
    typeof value.formatVersion === 'string' && LEGACY_LEDGER_FORMATS.has(value.formatVersion)
}

export async function readLegacyCompatibleNameStatus(
  database: IDBDatabase,
  operationId: string,
): Promise<boolean> {
  const transaction = database.transaction(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE, 'readonly')
  const completion = transactionCompletion(transaction)
  const row = await requestResult(
    transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE).get(operationId),
  )
  await completion
  return isLegacyCompatibleNameRecord(row, operationId)
}

export async function forgetLegacyCompatibleNameRecord(
  descriptor: ReceiveOperationResumeDescriptor,
  options: {
    readonly databaseName?: string
    readonly manager?: BrowserLockManagerRuntime
  } = {},
): Promise<Readonly<{ kind: 'record-forgotten' }>> {
  if (descriptor.continuation !== 'cleanup-incompatible') {
    throw new DOMException('Only incompatible saved records can be forgotten', 'InvalidStateError')
  }
  const manager = options.manager ?? globalThis.navigator?.locks
  if (manager === undefined) {
    throw new DOMException('Saved-record cleanup requires browser locks', 'NotSupportedError')
  }
  const expectedLifecycle = await storedReceiveLifecycleState(descriptor.lifecycle)
  await manager.request(
    browserReceiveOperationLockName(descriptor.operationId),
    { mode: 'exclusive', ifAvailable: true },
    async lock => {
      if (lock === null) throw new BrowserReceiveOperationBusyError(descriptor.operationId)
      const database = await openIndexedDbCheckpointDatabase(options.databaseName ?? DEFAULT_OUTPUT_DATABASE_NAME)
      try {
        const transaction = database.transaction(
          INDEXEDDB_V10_STORE_SCHEMAS.map(schema => schema.name),
          'readwrite',
        )
        const completion = transactionCompletion(transaction)
        try {
          const [header, lifecycle] = await Promise.all([
            requestResult(transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
              .get(descriptor.operationId)),
            requestResult(transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
              .get(operationRecordId(descriptor.operationId, RECEIVE_RECORD_LIFECYCLE_STATE))),
          ])
          if (!isLegacyCompatibleNameRecord(header, descriptor.operationId) ||
              !isIndexedDbRecord(lifecycle) || lifecycle.digest !== expectedLifecycle.digest ||
              lifecycle.lifecycleGeneration !== expectedLifecycle.lifecycleGeneration) {
            throw new DOMException('Saved record changed since inventory was opened', 'InvalidStateError')
          }
          // One transaction removes every operation-scoped row, including handle
          // references, so a crash cannot strand a resumable partial projection.
          await Promise.all(INDEXEDDB_V10_STORE_SCHEMAS.map(schema => {
            const store = transaction.objectStore(schema.name)
            if (schema.name === INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE) {
              return requestResult(store.delete(descriptor.operationId))
            }
            return deleteOperationRows(store, descriptor.operationId)
          }))
          await completion
        } catch (error) {
          try { transaction.abort() } catch { /* The transaction may already have aborted. */ }
          await completion.catch(() => undefined)
          throw error
        }
      } finally {
        database.close()
      }
    },
  )
  return Object.freeze({ kind: 'record-forgotten' })
}

function deleteOperationRows(store: IDBObjectStore, operationId: string): Promise<void> {
  const request = store.index(INDEXEDDB_BY_OPERATION_INDEX)
    .openKeyCursor(IDBKeyRange.only(operationId))
  return new Promise((resolve, reject) => {
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) {
        resolve()
        return
      }
      store.delete(cursor.primaryKey)
      cursor.continue()
    })
  })
}
