import {
  validatePersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../workspace/records'
import {
  RECEIVE_STATE_DOWNLOAD_STARTED,
  RECEIVE_STATE_EXPIRED,
  RECEIVE_STATE_NEEDS_ATTENTION,
  RECEIVE_STATE_PUBLISHED,
  RECEIVE_STATE_RESUMABLE_PACKAGE,
  RECEIVE_STATE_RESUMABLE_RECEIVE,
  RECEIVE_STATE_WAITING_TO_SAVE,
  type ReceiveLifecycleState,
} from '../workspace/state'
import { decodeStoredReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveOperationResumeSource } from '../resume/authority'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_BY_STATE_INDEX,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'

const RESUME_INVENTORY_BOUND = 1_048_576
const INVENTORIED_STATE_BYTES = Object.freeze([
  RECEIVE_STATE_RESUMABLE_RECEIVE,
  RECEIVE_STATE_RESUMABLE_PACKAGE,
  RECEIVE_STATE_WAITING_TO_SAVE,
  RECEIVE_STATE_PUBLISHED,
  RECEIVE_STATE_DOWNLOAD_STARTED,
  RECEIVE_STATE_EXPIRED,
  RECEIVE_STATE_NEEDS_ATTENTION,
] as const)

/**
 * Production resume inventory reads only v6 lifecycle records. Legacy v5
 * descriptors are intentionally reachable solely through the certified cleaner.
 */
export class IndexedDbReceiveResumeSource implements ReceiveOperationResumeSource {
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase) {
    this.#database = database
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    databaseName = DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  ): Promise<IndexedDbReceiveResumeSource> {
    return new IndexedDbReceiveResumeSource(
      await openIndexedDbCheckpointDatabase(databaseName),
    )
  }

  async listLifecycleStates(): Promise<readonly ReceiveLifecycleState[]> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_RECEIVE_RECORD_STORE,
      'readonly',
    )
    const index = transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
      .index(INDEXEDDB_BY_STATE_INDEX)
    const records: PersistedReceiveRecord[] = []
    for (const state of INVENTORIED_STATE_BYTES) {
      const remaining = RESUME_INVENTORY_BOUND + 1 - records.length
      records.push(...await requestResult<PersistedReceiveRecord[]>(index.getAll(
        IDBKeyRange.only(state),
        remaining,
      )))
      if (records.length > RESUME_INVENTORY_BOUND) break
    }
    await transactionCompletion(transaction)
    if (records.length > RESUME_INVENTORY_BOUND) {
      throw new DOMException('Receive resume inventory exceeds its bound', 'QuotaExceededError')
    }

    const states = await Promise.all(records.map(validateLifecycleRecord))
    const operations = new Set<string>()
    for (const state of states) {
      if (operations.has(state.operationId)) {
        throw new TypeError('Receive resume inventory contains duplicate lifecycle authority')
      }
      operations.add(state.operationId)
    }
    states.sort((left, right) => left.operationId.localeCompare(right.operationId))
    return Object.freeze(states)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('Receive resume source is closed', 'InvalidStateError')
    }
  }
}

async function validateLifecycleRecord(
  input: PersistedReceiveRecord,
): Promise<ReceiveLifecycleState> {
  return decodeStoredReceiveLifecycleState(
    await validatePersistedReceiveRecord(input),
  )
}
