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
import { decodeBase64Url, encodeBase64Url } from '../../../../crypto/bytes'
import { chainDirectZipEpochDigestV1 } from '../../format'
import { validatePersistedReceiveRecord, validateReceiveOperationHandleRecord } from '../../../workspace/records'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../../../workspace/state-codec'
import type {
  DirectZipCandidatePromotionV1,
  DirectZipCandidateRetirementV1,
  DirectZipCommitCandidateV1,
  DirectZipImmutablePageV1,
  DirectZipJournalFenceV1,
  DirectZipRecoveryLifecycleCommitV1,
} from '../model'
import { createDirectZipStateRowV1, validateDirectZipCheckpointV1, validateDirectZipCommitCandidateV1, validateDirectZipImmutablePageV1 } from '../records'
import {
  DirectZipJournalConcurrencyError,
  abortQuietly,
  assertCandidateFence,
  assertLifecycleCheckpointCut,
  assertLifecycleForCheckpoint,
  assertPromotedCheckpointCut,
  assertRecoveryLifecycleCut,
  assertRetirementCheckpointCut,
  directZipPageStore,
  sameCandidateRow,
  samePageRow,
  samePersistedRecordRow,
  sameUsage,
  snapshotFence,
} from './authority'
import type { IndexedDbDirectZipJournalStorage } from './storage'

export class IndexedDbDirectZipCheckpointTransactions {
  readonly #storage: IndexedDbDirectZipJournalStorage

  constructor(storage: IndexedDbDirectZipJournalStorage) {
    this.#storage = storage
  }

  async stagePage(
    fenceInput: DirectZipJournalFenceV1,
    pageInput: DirectZipImmutablePageV1,
  ): Promise<void> {
    this.#storage.assertOpen()
    const fence = snapshotFence(fenceInput)
    const page = await validateDirectZipImmutablePageV1(pageInput)
    if (page.operationId !== fence.operationId) {
      throw new TypeError('Direct ZIP page escaped its fenced operation')
    }
    const expectedState = await this.#storage.readFenceState(fence)
    const predecessors = await this.#storage.assertPagePredecessors(page, fence)
    const pageStore = directZipPageStore(page.pageKind)
    const transaction = this.#storage.database.transaction([...new Set([
      pageStore,
      ...predecessors.map(predecessor => directZipPageStore(predecessor.pageKind)),
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ])], 'readwrite')
    try {
      const state = await this.#storage.assertFence(transaction, fence, expectedState)
      for (const predecessor of predecessors) {
        const stored = await requestResult<unknown>(transaction
          .objectStore(directZipPageStore(predecessor.pageKind)).get(predecessor.id))
        if (!samePageRow(stored, predecessor)) {
          throw new DirectZipJournalConcurrencyError('Direct ZIP page predecessor changed')
        }
      }
      const candidateCount = await requestResult(transaction
        .objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
        .index(INDEXEDDB_BY_OPERATION_INDEX).count(IDBKeyRange.only(fence.operationId)))
      if (candidateCount !== 0) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP page staging found a live candidate')
      }
      if (page.accountingPredecessor.kind === 'checkpoint' &&
          (page.accountingPredecessor.checkpointGeneration !== fence.checkpointGeneration ||
           page.accountingPredecessor.checkpointDigest !== state.checkpointDigest ||
           !sameUsage(page, state.checkpoint.journalUsage))) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP page accounting predecessor changed')
      }
      const store = transaction.objectStore(pageStore)
      const existing = await requestResult<unknown>(store.get(page.id))
      if (existing === undefined) store.add(page)
      else if (!samePageRow(existing, page)) {
        throw new DirectZipJournalConcurrencyError('immutable Direct ZIP page ID was reused')
      }
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.page_staged',
        operation_id: fence.operationId,
        lease_id: fence.leaseId,
        checkpoint_generation: fence.checkpointGeneration,
        page_kind: page.pageKind,
        page_ordinal: page.pageOrdinal,
      })
    } catch (error) {
      this.#storage.failed(fence.operationId, fence.leaseId, undefined, error)
      abortQuietly(transaction)
      throw error
    }
  }

  async bindCandidate(
    fenceInput: DirectZipJournalFenceV1,
    candidateInput: DirectZipCommitCandidateV1,
  ): Promise<void> {
    this.#storage.assertOpen()
    const fence = snapshotFence(fenceInput)
    const candidate = await validateDirectZipCommitCandidateV1(candidateInput)
    assertCandidateFence(candidate, fence)
    const expectedState = await this.#storage.readFenceState(fence)
    if (expectedState.checkpointDigest !== candidate.predecessorCheckpointDigest ||
        expectedState.checkpoint.targetObservation.digest !==
          candidate.predecessorTargetObservation.digest ||
        candidate.proposedCheckpoint.committedArchiveLength <=
          expectedState.checkpoint.committedArchiveLength ||
        (candidate.kind === 'closing' && candidate.proposedCheckpoint.closingReplay?.archiveOffset !==
          expectedState.checkpoint.committedArchiveLength) ||
        candidate.proposedCheckpoint.epochRootDigest !== encodeBase64Url(
          chainDirectZipEpochDigestV1({
            predecessorRoot: decodeBase64Url(expectedState.checkpoint.epochRootDigest)!,
            start: expectedState.checkpoint.committedArchiveLength,
            end: candidate.proposedCheckpoint.committedArchiveLength,
            contentDigest: decodeBase64Url(candidate.expectedRangeDigest)!,
          }),
        )) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP candidate predecessor changed')
    }
    const expectedPageTails = await this.#storage.checkpointPageTails(candidate.proposedCheckpoint)
    const transaction = this.#storage.database.transaction([
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
      INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
      INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
      INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
    ], 'readwrite')
    try {
      await this.#storage.assertFence(transaction, fence, expectedState)
      for (const page of expectedPageTails) {
        const current = await requestResult<unknown>(transaction
          .objectStore(directZipPageStore(page.pageKind)).get(page.id))
        if (!samePageRow(current, page)) {
          throw new DirectZipJournalConcurrencyError(
            'Direct ZIP candidate page authority changed during binding',
          )
        }
      }
      const store = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      const existingCount = await requestResult(store.index(INDEXEDDB_BY_OPERATION_INDEX)
        .count(IDBKeyRange.only(fence.operationId)))
      const existing = await requestResult<unknown>(store.get(candidate.id))
      if (existing !== undefined) {
        if (!sameCandidateRow(existing, candidate) || existingCount !== 1) {
          throw new DirectZipJournalConcurrencyError('Direct ZIP candidate authority conflicts')
        }
      } else {
        if (existingCount !== 0) {
          throw new DirectZipJournalConcurrencyError('Direct ZIP operation already owns a candidate')
        }
        store.add(candidate)
      }
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.candidate_bound',
        operation_id: fence.operationId,
        lease_id: fence.leaseId,
        checkpoint_generation: fence.checkpointGeneration,
        candidate_id: candidate.candidateId,
      })
    } catch (error) {
      this.#storage.failed(fence.operationId, fence.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }

  async promoteCandidate(cut: DirectZipCandidatePromotionV1): Promise<void> {
    this.#storage.assertOpen()
    const fence = snapshotFence(cut.fence)
    const candidate = await validateDirectZipCommitCandidateV1(cut.candidate)
    const checkpoint = await validateDirectZipCheckpointV1(cut.checkpoint)
    assertCandidateFence(candidate, fence)
    const lifecycleRecord = await validatePersistedReceiveRecord(cut.lifecycleRecord)
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
    const lifecycleProjection = await storedReceiveLifecycleState(cut.lifecycle)
    const handles = (cut.handles ?? []).map(validateReceiveOperationHandleRecord)
    if (!samePersistedRecordRow(lifecycleProjection, lifecycleRecord) ||
        lifecycle.operationId !== fence.operationId ||
        lifecycle.receiveIntentDigest !== checkpoint.receiveIntentDigest ||
        handles.some(handle => handle.operationId !== fence.operationId)) {
      throw new TypeError('Direct ZIP candidate promotion lifecycle authority disagrees')
    }
    const expectedState = await this.#storage.readFenceState(fence)
    if (expectedState.checkpointDigest !== candidate.predecessorCheckpointDigest) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP promotion predecessor changed')
    }
    const currentLifecycleValue = await this.#storage.read<unknown>(
      INDEXEDDB_RECEIVE_RECORD_STORE,
      lifecycleRecord.id,
    )
    if (currentLifecycleValue === undefined) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle authority is absent')
    }
    const currentLifecycleRecord = await validatePersistedReceiveRecord(
      currentLifecycleValue as import('../../../workspace/records').PersistedReceiveRecord,
    )
    const currentLifecycle = decodeStoredReceiveLifecycleState(currentLifecycleRecord)
    if (currentLifecycle.generation + 1n !== lifecycle.generation) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle generation changed')
    }
    assertPromotedCheckpointCut(checkpoint, candidate)
    assertLifecycleCheckpointCut(lifecycle, checkpoint, undefined)
    const state = await createDirectZipStateRowV1(
      checkpoint,
      fence.leaseId,
    )
    const transaction = this.#storage.database.transaction([
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite')
    try {
      await this.#storage.assertFence(transaction, fence, expectedState)
      const storedCandidate = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).get(candidate.id))
      if (!sameCandidateRow(storedCandidate, candidate)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP promotion candidate changed')
      }
      const storedLifecycle = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).get(lifecycleRecord.id))
      if (!samePersistedRecordRow(storedLifecycle, currentLifecycleRecord)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle authority changed')
      }
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).put(state)
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).put(lifecycleRecord)
      for (const handle of handles) {
        transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE).put(handle)
      }
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).delete(candidate.id)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.candidate_promoted',
        operation_id: fence.operationId,
        lease_id: fence.leaseId,
        checkpoint_generation: state.checkpoint.generation,
        candidate_id: candidate.candidateId,
      })
    } catch (error) {
      this.#storage.failed(fence.operationId, fence.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }

  async commitRecoveryLifecycle(cut: DirectZipRecoveryLifecycleCommitV1): Promise<void> {
    this.#storage.assertOpen()
    const fence = snapshotFence(cut.fence)
    const candidate = cut.candidate === undefined
      ? undefined
      : await validateDirectZipCommitCandidateV1(cut.candidate)
    if (candidate !== undefined) assertCandidateFence(candidate, fence)
    const lifecycleRecord = await validatePersistedReceiveRecord(cut.lifecycleRecord)
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
    const lifecycleProjection = await storedReceiveLifecycleState(cut.lifecycle)
    const expectedState = await this.#storage.readFenceState(fence)
    if (candidate !== undefined &&
        expectedState.checkpointDigest !== candidate.predecessorCheckpointDigest) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP recovery candidate predecessor changed')
    }
    if (!samePersistedRecordRow(lifecycleProjection, lifecycleRecord) ||
        lifecycle.operationId !== fence.operationId ||
        lifecycle.receiveIntentDigest !== expectedState.checkpoint.receiveIntentDigest) {
      throw new TypeError('Direct ZIP recovery lifecycle escaped its operation')
    }
    const currentLifecycleValue = await this.#storage.read<unknown>(
      INDEXEDDB_RECEIVE_RECORD_STORE,
      lifecycleRecord.id,
    )
    if (currentLifecycleValue === undefined) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle authority is absent')
    }
    const currentLifecycleRecord = await validatePersistedReceiveRecord(
      currentLifecycleValue as import('../../../workspace/records').PersistedReceiveRecord,
    )
    const currentLifecycle = decodeStoredReceiveLifecycleState(currentLifecycleRecord)
    if (currentLifecycle.generation + 1n !== lifecycle.generation) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle generation changed')
    }
    assertRecoveryLifecycleCut(lifecycle, expectedState, fence, cut.recoveryGate, candidate)
    const state = await createDirectZipStateRowV1(
      expectedState.checkpoint,
      fence.leaseId,
      cut.recoveryGate,
    )
    const transaction = this.#storage.database.transaction([
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite')
    try {
      await this.#storage.assertFence(transaction, fence, expectedState)
      const candidateStore = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      const candidateCount = await requestResult(candidateStore.index(INDEXEDDB_BY_OPERATION_INDEX)
        .count(IDBKeyRange.only(fence.operationId)))
      const candidateValue = candidate === undefined
        ? undefined
        : await requestResult<unknown>(candidateStore.get(candidate.id))
      if ((candidate === undefined && candidateCount !== 0) ||
          (candidate !== undefined &&
           (candidateCount !== 1 || !sameCandidateRow(candidateValue, candidate)))) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP recovery candidate changed')
      }
      const storedLifecycle = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).get(lifecycleRecord.id))
      if (!samePersistedRecordRow(storedLifecycle, currentLifecycleRecord)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle authority changed')
      }
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).put(state)
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).put(lifecycleRecord)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.recovery_lifecycle_committed',
        operation_id: fence.operationId,
        lease_id: fence.leaseId,
        checkpoint_generation: fence.checkpointGeneration,
        ...(candidate === undefined ? {} : { candidate_id: candidate.candidateId }),
        decision: lifecycle.kind,
      })
    } catch (error) {
      this.#storage.failed(fence.operationId, fence.leaseId, candidate?.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }

  async retireCandidate(cut: DirectZipCandidateRetirementV1): Promise<void> {
    this.#storage.assertOpen()
    const fence = snapshotFence(cut.fence)
    const candidate = await validateDirectZipCommitCandidateV1(cut.candidate)
    assertCandidateFence(candidate, fence)
    const checkpoint = await validateDirectZipCheckpointV1(cut.checkpoint)
    const lifecycleRecord = await validatePersistedReceiveRecord(cut.lifecycleRecord)
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
    const lifecycleProjection = await storedReceiveLifecycleState(cut.lifecycle)
    const expectedState = await this.#storage.readFenceState(fence)
    if (expectedState.checkpointDigest !== candidate.predecessorCheckpointDigest) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP retirement predecessor changed')
    }
    if (!samePersistedRecordRow(lifecycleProjection, lifecycleRecord)) {
      throw new TypeError('Direct ZIP retirement lifecycle projection disagrees')
    }
    assertRetirementCheckpointCut(cut.disposition, expectedState, checkpoint, candidate)
    assertLifecycleForCheckpoint(lifecycle, checkpoint, fence)
    const currentLifecycleValue = await this.#storage.read<unknown>(
      INDEXEDDB_RECEIVE_RECORD_STORE,
      lifecycleRecord.id,
    )
    if (currentLifecycleValue === undefined) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle authority is absent')
    }
    const currentLifecycleRecord = await validatePersistedReceiveRecord(
      currentLifecycleValue as import('../../../workspace/records').PersistedReceiveRecord,
    )
    const currentLifecycle = decodeStoredReceiveLifecycleState(currentLifecycleRecord)
    if (currentLifecycle.generation + 1n !== lifecycle.generation) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle generation changed')
    }
    const state = await createDirectZipStateRowV1(checkpoint, fence.leaseId)
    const transaction = this.#storage.database.transaction([
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite')
    try {
      await this.#storage.assertFence(transaction, fence, expectedState)
      const storedCandidate = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).get(candidate.id))
      if (!sameCandidateRow(storedCandidate, candidate)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP retirement candidate changed')
      }
      const storedLifecycle = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).get(lifecycleRecord.id))
      if (!samePersistedRecordRow(storedLifecycle, currentLifecycleRecord)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle authority changed')
      }
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).put(state)
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).put(lifecycleRecord)
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).delete(candidate.id)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.candidate_retired',
        operation_id: fence.operationId,
        lease_id: fence.leaseId,
        checkpoint_generation: checkpoint.generation,
        candidate_id: candidate.candidateId,
        decision: cut.disposition,
      })
    } catch (error) {
      this.#storage.failed(fence.operationId, fence.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }


}
