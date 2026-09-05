import {
  RECEIVE_RECORD_OPERATION,
  decodeStoredReceiveOperation,
  operationRecordId,
  validatePersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../workspace/records'
import {
  RECEIVE_STATE_AUTHORIZATION_REQUIRED,
  RECEIVE_STATE_DESTINATION_SPACE_REQUIRED,
  RECEIVE_STATE_DOWNLOAD_STARTED,
  RECEIVE_STATE_EXPIRED,
  RECEIVE_STATE_NEEDS_ATTENTION,
  RECEIVE_STATE_PARTIAL_DIRECTORY,
  RECEIVE_STATE_PUBLISHED,
  RECEIVE_STATE_RECEIVING,
  RECEIVE_STATE_RESUMABLE_PACKAGE,
  RECEIVE_STATE_RESUMABLE_RECEIVE,
  RECEIVE_STATE_WAITING_TO_SAVE,
  RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED,
  type ReceiveLifecycleState,
} from '../workspace/state'
import { decodeStoredReceiveLifecycleState } from '../workspace/state-codec'
import { readLegacyCompatibleNameStatus } from './indexeddb/compatible-name-legacy-cleanup'
import type { ReceiveOperationResumeSource } from '../resume/authority'
import type { RecoverySummary } from '../file-system-access/recovery-summary'
import { readFSARecoverySummary } from '../file-system-access/recovery-summary'
import { openFSAFileCheckpointRepository } from '../file-system-access/checkpoint-repository'
import { requireDirectTreeIntent } from '../file-system-access/settlement-proof'
import { FSA_RESERVED_ROOT_LAYOUT_VERSION } from '../../transfer/intent'
import {
  DIRECT_ZIP_CANDIDATE_BOOTSTRAP,
  type DirectZipBootstrapCandidateV1,
} from '../direct-zip/journal/model'
import {
  directZipBootstrapResumeDescriptorV1,
  type DirectZipBootstrapResumeDescriptorV1,
} from '../direct-zip/journal/repository'
import { validateDirectZipBootstrapCandidateV1 } from '../direct-zip/journal/records'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_BY_STATE_INDEX,
  INDEXEDDB_BY_KIND_CANDIDATE_INDEX,
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'

const RESUME_INVENTORY_BOUND = 1_048_576
const INVENTORIED_STATE_BYTES = Object.freeze([
  RECEIVE_STATE_RECEIVING,
  RECEIVE_STATE_RESUMABLE_RECEIVE,
  RECEIVE_STATE_RESUMABLE_PACKAGE,
  RECEIVE_STATE_WAITING_TO_SAVE,
  RECEIVE_STATE_PUBLISHED,
  RECEIVE_STATE_PARTIAL_DIRECTORY,
  RECEIVE_STATE_DOWNLOAD_STARTED,
  RECEIVE_STATE_EXPIRED,
  RECEIVE_STATE_NEEDS_ATTENTION,
  RECEIVE_STATE_AUTHORIZATION_REQUIRED,
  RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED,
  RECEIVE_STATE_DESTINATION_SPACE_REQUIRED,
] as const)

/**
 * Production inventory reads only strict V2 lifecycle records. The v9 migration
 * removes older receive authority before this source can enumerate it.
 */
export class IndexedDbReceiveResumeSource implements ReceiveOperationResumeSource {
  readonly #database: IDBDatabase
  readonly #databaseName: string
  #closed = false

  private constructor(database: IDBDatabase, databaseName: string) {
    this.#database = database
    this.#databaseName = databaseName
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    databaseName = DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  ): Promise<IndexedDbReceiveResumeSource> {
    return new IndexedDbReceiveResumeSource(
      await openIndexedDbCheckpointDatabase(databaseName),
      databaseName,
    )
  }

  async listDirectZipBootstrapCandidates(): Promise<readonly DirectZipBootstrapResumeDescriptorV1[]> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      'readonly',
    )
    const index = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      .index(INDEXEDDB_BY_KIND_CANDIDATE_INDEX)
    const values = await collectCursorValues(
      index.openCursor(IDBKeyRange.bound(
        [DIRECT_ZIP_CANDIDATE_BOOTSTRAP, ''],
        [DIRECT_ZIP_CANDIDATE_BOOTSTRAP, '\uffff'],
      )),
      RESUME_INVENTORY_BOUND + 1,
    )
    await transactionCompletion(transaction)
    if (values.length > RESUME_INVENTORY_BOUND) {
      throw new DOMException('Direct ZIP bootstrap inventory exceeds its bound', 'QuotaExceededError')
    }
    const candidates = await Promise.all(values.map(value =>
      validateDirectZipBootstrapCandidateV1(value as DirectZipBootstrapCandidateV1)))
    return Object.freeze(candidates.map(directZipBootstrapResumeDescriptorV1))
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
      records.push(...await collectCursorValues(
        index.openCursor(IDBKeyRange.only(state), 'next'),
        remaining,
      ) as PersistedReceiveRecord[])
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

  async isCleanupOnly(operationId: string): Promise<boolean> {
    this.#assertOpen()
    return readLegacyCompatibleNameStatus(this.#database, operationId)
  }

  async readRecoverySummary(
    lifecycle: Extract<ReceiveLifecycleState, {
      kind: 'resumable-receive'
      payloadKind: 'file-set'
    }>,
  ): Promise<RecoverySummary | undefined> {
    this.#assertOpen()
    const transaction = this.#database.transaction(INDEXEDDB_RECEIVE_RECORD_STORE, 'readonly')
    const stored = await requestResult(transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).get(
      operationRecordId(lifecycle.operationId, RECEIVE_RECORD_OPERATION),
    ))
    await transactionCompletion(transaction)
    if (stored === undefined) {
      throw new TypeError('FSA recovery summary requires its persisted receive operation')
    }
    const operation = await decodeStoredReceiveOperation(stored as PersistedReceiveRecord)
    if (operation.receiveIntent.operationId !== lifecycle.operationId ||
        operation.receiveIntent.digest !== lifecycle.receiveIntentDigest) {
      throw new TypeError('FSA recovery lifecycle does not belong to its persisted receive intent')
    }
    if (operation.receiveIntent.plan.kind !== 'direct-tree') return undefined
    const intent = await requireDirectTreeIntent(operation.receiveIntent)
    const reservation = intent.plan.reservation
    if (reservation.kind !== 'named-container-entry' ||
        !('fsaLayoutVersion' in reservation) ||
        reservation.fsaLayoutVersion !== FSA_RESERVED_ROOT_LAYOUT_VERSION) {
      throw new TypeError('FSA recovery summary requires the reserved-root destination binding')
    }

    const checkpoints = await openFSAFileCheckpointRepository(
      { databaseName: this.#databaseName },
      intent,
      reservation,
    )
    try {
      return readFSARecoverySummary({
        intent,
        lifecycle,
        checkpoints,
      })
    } finally {
      checkpoints.close()
    }
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

function collectCursorValues(
  request: IDBRequest<IDBCursorWithValue | null>,
  limit: number,
): Promise<unknown[]> {
  return new Promise((resolve, reject) => {
    const values: unknown[] = []
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null || values.length >= limit) {
        resolve(values)
        return
      }
      values.push(cursor.value)
      if (values.length >= limit) resolve(values)
      else cursor.continue()
    })
  })
}

async function validateLifecycleRecord(
  input: PersistedReceiveRecord,
): Promise<ReceiveLifecycleState> {
  return decodeStoredReceiveLifecycleState(
    await validatePersistedReceiveRecord(input),
  )
}
