import {
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
  INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_STATE_STORE,
  INDEXEDDB_RECEIVE_HANDLE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  requestResult,
  transactionCompletion,
} from '../../../browser/indexeddb-database'
import { validatePersistedReceiveRecord, validateReceiveOperationHandleRecord } from '../../../workspace/records'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../../../workspace/state-codec'
import type { DirectZipJournalFenceV1, DirectZipRollbackCandidateV1, DirectZipRollbackPromotionV1 } from '../model'
import { createDirectZipStateRowV1, validateDirectZipCheckpointV1 } from '../records'
import { assertDirectZipRollbackPredecessorV1, validateDirectZipRollbackCandidateV1 } from '../rollback'
import {
  abortQuietly, assertCandidateCheckpointFence, assertCandidateFence,
  assertLifecycleForCheckpoint, DirectZipJournalConcurrencyError, directZipPageStore,
  sameCandidateRow, sameCheckpointResumeAuthority, samePageRow, samePersistedRecordRow, snapshotFence,
} from './authority'
import type { IndexedDbDirectZipJournalStorage } from './storage'

const ROLLBACK_AUTHORITY_STORES = [
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
  INDEXEDDB_DIRECT_ZIP_STATE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
]

/** The intent survives file truncation; only its observed result advances durable progress. */
export class IndexedDbDirectZipRollbackTransactions {
  readonly #storage: IndexedDbDirectZipJournalStorage

  constructor(storage: IndexedDbDirectZipJournalStorage) { this.#storage = storage }

  async bindRollbackCandidate(
    fenceInput: DirectZipJournalFenceV1,
    input: DirectZipRollbackCandidateV1,
  ): Promise<void> {
    this.#storage.assertOpen()
    const fence = snapshotFence(fenceInput)
    const candidate = await validateDirectZipRollbackCandidateV1(input)
    assertCandidateFence(candidate, fence)
    const expectedState = await this.#storage.readFenceState(fence)
    assertDirectZipRollbackPredecessorV1(expectedState.checkpoint, candidate)
    const pageTails = await this.#storage.checkpointPageTails(candidate.proposedCheckpoint)
    const transaction = this.#storage.database.transaction(ROLLBACK_AUTHORITY_STORES, 'readwrite')
    try {
      await this.#storage.assertFence(transaction, fence, expectedState)
      for (const page of pageTails) {
        const current = await requestResult<unknown>(transaction
          .objectStore(directZipPageStore(page.pageKind)).get(page.id))
        if (!samePageRow(current, page)) {
          throw new DirectZipJournalConcurrencyError('Direct ZIP rollback prefix authority changed')
        }
      }
      const store = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      const count = await requestResult(store.index(INDEXEDDB_BY_OPERATION_INDEX)
        .count(IDBKeyRange.only(fence.operationId)))
      const existing = await requestResult<unknown>(store.get(candidate.id))
      if (existing === undefined) {
        if (count !== 0) throw new DirectZipJournalConcurrencyError('Direct ZIP operation already owns a candidate')
        store.add(candidate)
      } else if (count !== 1 || !sameCandidateRow(existing, candidate)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP rollback candidate authority conflicts')
      }
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.rollback_bound', operation_id: fence.operationId,
        lease_id: fence.leaseId, checkpoint_generation: fence.checkpointGeneration,
        candidate_id: candidate.candidateId, decision: 'truncate-active-member',
      })
    } catch (error) {
      this.#storage.failed(fence.operationId, fence.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }

  async promoteRollbackCandidate(cut: DirectZipRollbackPromotionV1): Promise<void> {
    this.#storage.assertOpen()
    const fence = snapshotFence(cut.fence)
    const candidate = await validateDirectZipRollbackCandidateV1(cut.candidate)
    assertCandidateCheckpointFence(candidate, fence)
    const checkpoint = await validateDirectZipCheckpointV1(cut.checkpoint)
    const expectedState = await this.#storage.readFenceState(fence)
    assertDirectZipRollbackPredecessorV1(expectedState.checkpoint, candidate)
    if (checkpoint.generation !== candidate.proposedCheckpoint.generation ||
        checkpoint.predecessorCheckpointDigest !== candidate.predecessorCheckpointDigest ||
        checkpoint.candidateLineageDigest !== candidate.digest ||
        checkpoint.targetObservation.ownershipMarkerDigest !==
          candidate.predecessorTargetObservation.ownershipMarkerDigest ||
        !sameCheckpointResumeAuthority(candidate.proposedCheckpoint, checkpoint)) {
      throw new TypeError('Direct ZIP rollback promotion did not bind its restored observed checkpoint')
    }
    const lifecycleRecord = await validatePersistedReceiveRecord(cut.lifecycleRecord)
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
    const projection = await storedReceiveLifecycleState(cut.lifecycle)
    if (!samePersistedRecordRow(projection, lifecycleRecord)) {
      throw new TypeError('Direct ZIP rollback lifecycle projection disagrees')
    }
    assertLifecycleForCheckpoint(lifecycle, checkpoint, fence)
    const handles = (cut.handles ?? []).map(validateReceiveOperationHandleRecord)
    if (handles.some(handle => handle.operationId !== fence.operationId)) {
      throw new TypeError('Direct ZIP rollback handles escaped their operation')
    }
    const lifecycleValue = await this.#storage.read<unknown>(INDEXEDDB_RECEIVE_RECORD_STORE, lifecycleRecord.id)
    if (lifecycleValue === undefined) throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle authority is absent')
    const previousLifecycleRecord = await validatePersistedReceiveRecord(
      lifecycleValue as import('../../../workspace/records').PersistedReceiveRecord,
    )
    if (decodeStoredReceiveLifecycleState(previousLifecycleRecord).generation + 1n !== lifecycle.generation) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP rollback lifecycle generation changed')
    }
    const state = await createDirectZipStateRowV1(checkpoint, fence.leaseId)
    const transaction = this.#storage.database.transaction([
      ...ROLLBACK_AUTHORITY_STORES, INDEXEDDB_RECEIVE_RECORD_STORE, INDEXEDDB_RECEIVE_HANDLE_STORE,
    ], 'readwrite')
    try {
      await this.#storage.assertFence(transaction, fence, expectedState)
      const store = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      const count = await requestResult(store.index(INDEXEDDB_BY_OPERATION_INDEX)
        .count(IDBKeyRange.only(fence.operationId)))
      const storedCandidate = await requestResult<unknown>(store.get(candidate.id))
      if (count !== 1 || !sameCandidateRow(storedCandidate, candidate)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP rollback candidate changed')
      }
      const storedLifecycle = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).get(lifecycleRecord.id))
      if (!samePersistedRecordRow(storedLifecycle, previousLifecycleRecord)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP rollback lifecycle authority changed')
      }
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).put(state)
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).put(lifecycleRecord)
      for (const handle of handles) transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE).put(handle)
      store.delete(candidate.id)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.rollback_promoted', operation_id: fence.operationId,
        lease_id: fence.leaseId, checkpoint_generation: checkpoint.generation,
        candidate_id: candidate.candidateId, decision: 'replay-active-member',
      })
    } catch (error) {
      this.#storage.failed(fence.operationId, fence.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }
}
