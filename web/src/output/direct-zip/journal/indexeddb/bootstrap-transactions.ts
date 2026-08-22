import {
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
  INDEXEDDB_DIRECT_ZIP_STATE_STORE,
  INDEXEDDB_RECEIVE_HANDLE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  requestResult,
  transactionCompletion,
} from '../../../browser/indexeddb-database'
import {
  operationRecordId,
  RECEIVE_RECORD_OPERATION,
  decodeStoredReceiveOperation,
  storedReceiveOperationRecord,
  validatePersistedReceiveRecord,
  validateReceiveOperationHandleRecord,
  validateReceiveOperationLeaseRecord,
  receiveOperationLeaseId,
  type ReceiveOperationLeaseRecord,
} from '../../../workspace/records'
import { equalCanonicalBytes } from '../../../workspace/canonical'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../../../workspace/state-codec'
import { prepareReceiveOperationTransition, type ReceiveOperationTransition } from '../../../workspace/repository'
import { applyOperationTransition, assertOperationConcurrency, assertOperationMutationOwnership } from '../../../browser/indexeddb/repository-transactions'
import type { DirectZipBootstrapCommitV1 } from '../model'
import { createDirectZipStateRowV1, directZipStateId, validateDirectZipBootstrapCandidateV1, validateDirectZipCheckpointV1 } from '../records'
import type { DirectZipBootstrapCandidateCutV1, DirectZipBootstrapLeaseReplacementV1 } from '../repository'
import {
  DirectZipJournalConcurrencyError,
  abortQuietly,
  isHandleForOperation,
  sameBootstrapReservationAuthority,
  sameCandidateRow,
  sameDirectZipPolicies,
  sameJournalPolicies,
  samePersistedRecordRow,
  sameRanking,
  sameStateRow,
} from './authority'
import type { IndexedDbDirectZipJournalStorage } from './storage'

export class IndexedDbDirectZipBootstrapTransactions {
  readonly #storage: IndexedDbDirectZipJournalStorage

  constructor(storage: IndexedDbDirectZipJournalStorage) {
    this.#storage = storage
  }

  async commitLeaseAcquisition(transitionInput: ReceiveOperationTransition): Promise<void> {
    this.#storage.assertOpen()
    const transition = await prepareReceiveOperationTransition(transitionInput)
    if (transition.lease?.kind !== 'put') {
      throw new TypeError('Direct ZIP lease acquisition must install a durable lease')
    }
    const expectedState = await this.#storage.readState(transition.operationId)
    if (expectedState === undefined) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lease acquisition lost its state')
    }
    if (transition.expectedLeaseId !== undefined &&
        expectedState.leaseId !== transition.expectedLeaseId) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP lease authorities disagree')
    }
    const state = await createDirectZipStateRowV1(
      expectedState.checkpoint,
      transition.lease.record.leaseId,
      expectedState.recoveryGate,
    )
    const transaction = this.#storage.database.transaction([
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
      INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
    ], 'readwrite')
    try {
      await assertOperationConcurrency(transaction, transition)
      await assertOperationMutationOwnership(transaction, transition)
      const storedState = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).get(expectedState.id))
      if (!sameStateRow(storedState, expectedState)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP state changed during lease acquisition')
      }
      applyOperationTransition(transaction, transition)
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).put(state)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.lease_acquired',
        operation_id: transition.operationId,
        lease_id: transition.lease.record.leaseId,
        checkpoint_generation: expectedState.checkpoint.generation,
      })
    } catch (error) {
      this.#storage.failed(
        transition.operationId,
        transition.lease.record.leaseId,
        undefined,
        error,
      )
      abortQuietly(transaction)
      throw error
    }
  }

  async createBootstrapCandidate(cut: DirectZipBootstrapCandidateCutV1): Promise<void> {
    this.#storage.assertOpen()
    const candidate = await validateDirectZipBootstrapCandidateV1(cut.candidate)
    const handle = validateReceiveOperationHandleRecord(cut.provisionalParentHandle)
    const lease = validateReceiveOperationLeaseRecord(cut.lease)
    if (handle.operationId !== candidate.operationId || handle.id !== candidate.parentHandleId ||
        lease.operationId !== candidate.operationId || lease.leaseId !== candidate.leaseId) {
      throw new TypeError('Direct ZIP bootstrap cut does not share one authority')
    }
    const stores = [
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ] as const
    const transaction = this.#storage.database.transaction(stores, 'readwrite')
    try {
      const [candidateCount, state, operation, existingLease] = await Promise.all([
        requestResult(transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
          .index(INDEXEDDB_BY_OPERATION_INDEX).count(IDBKeyRange.only(candidate.operationId))),
        requestResult(transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE)
          .get(directZipStateId(candidate.operationId))),
        requestResult(transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
          .get(operationRecordId(candidate.operationId, RECEIVE_RECORD_OPERATION))),
        requestResult(transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE).get(lease.id)),
      ])
      if (candidateCount !== 0 || state !== undefined || operation !== undefined ||
          existingLease !== undefined) {
        throw new DirectZipJournalConcurrencyError(
          'Direct ZIP bootstrap operation already owns durable authority',
        )
      }
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).add(candidate)
      transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE).add(handle)
      transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE).add(lease)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.bootstrap_stored',
        operation_id: candidate.operationId,
        lease_id: candidate.leaseId,
        candidate_id: candidate.candidateId,
      })
    } catch (error) {
      this.#storage.failed(candidate.operationId, candidate.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }

  async replaceBootstrapLease(cut: DirectZipBootstrapLeaseReplacementV1): Promise<void> {
    this.#storage.assertOpen()
    const expected = await validateDirectZipBootstrapCandidateV1(cut.expectedCandidate)
    const candidate = await validateDirectZipBootstrapCandidateV1(cut.candidate)
    const lease = validateReceiveOperationLeaseRecord(cut.lease)
    if (!sameBootstrapReservationAuthority(expected, candidate) ||
        candidate.leaseGeneration !== expected.leaseGeneration + 1n ||
        candidate.leaseId === expected.leaseId || lease.operationId !== candidate.operationId ||
        lease.leaseId !== candidate.leaseId) {
      throw new TypeError('Direct ZIP bootstrap lease replacement changed reservation authority')
    }
    const transaction = this.#storage.database.transaction([
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite')
    try {
      const [storedCandidate, state, operation, storedHandle, storedLease] = await Promise.all([
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
          .get(expected.id)),
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE)
          .get(directZipStateId(expected.operationId))),
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
          .get(operationRecordId(expected.operationId, RECEIVE_RECORD_OPERATION))),
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE)
          .get(expected.parentHandleId)),
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE)
          .get(receiveOperationLeaseId(expected.operationId))),
      ])
      const currentLease = storedLease === undefined
        ? undefined
        : validateReceiveOperationLeaseRecord(storedLease as ReceiveOperationLeaseRecord)
      if (!sameCandidateRow(storedCandidate, expected) || state !== undefined ||
          operation !== undefined ||
          !isHandleForOperation(storedHandle, expected.operationId, expected.parentHandleId) ||
          currentLease?.leaseId !== expected.leaseId) {
        throw new DirectZipJournalConcurrencyError(
          'Direct ZIP bootstrap authority changed during lease replacement',
        )
      }
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).put(candidate)
      transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE).put(lease)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.bootstrap_lease_replaced',
        operation_id: candidate.operationId,
        lease_id: candidate.leaseId,
        candidate_id: candidate.candidateId,
        decision: candidate.leaseGeneration.toString(10),
      })
    } catch (error) {
      this.#storage.failed(candidate.operationId, candidate.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }

  async commitBootstrap(cut: DirectZipBootstrapCommitV1): Promise<void> {
    this.#storage.assertOpen()
    const candidate = await validateDirectZipBootstrapCandidateV1(cut.candidate)
    const checkpoint = await validateDirectZipCheckpointV1(cut.checkpoint)
    const operationRecord = await validatePersistedReceiveRecord(cut.operationRecord)
    const lifecycleRecord = await validatePersistedReceiveRecord(cut.lifecycleRecord)
    const operation = await decodeStoredReceiveOperation(operationRecord)
    const operationProjection = storedReceiveOperationRecord(operation)
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
    const lifecycleProjection = await storedReceiveLifecycleState(cut.lifecycle)
    const lease = validateReceiveOperationLeaseRecord(cut.lease)
    const handles = cut.handles.map(validateReceiveOperationHandleRecord)
    if (checkpoint.operationId !== candidate.operationId || checkpoint.generation !== 1n ||
        checkpoint.predecessorCheckpointDigest !== undefined || checkpoint.phase !== 'between-members' ||
        checkpoint.journalUsage.memberCount !== 0n ||
        checkpoint.journalUsage.canonicalMetadataBytes !== 0n ||
        operationProjection.digest !== operationRecord.digest ||
        !samePersistedRecordRow(lifecycleProjection, lifecycleRecord) ||
        cut.operation.digest !== operation.digest ||
        operation.operationId !== candidate.operationId ||
        operation.receiveIntent.plan.kind !== 'direct-resumable-zip' ||
        operation.choiceId !== candidate.choiceId ||
        !sameRanking(operation.preClickRanking, candidate.preClickRanking) ||
        !equalCanonicalBytes(
          operation.receiveIntent.selection.canonicalBytes,
          candidate.selectionCanonicalBytes,
        ) ||
        !equalCanonicalBytes(
          operation.receiveIntent.artifact.canonicalBytes,
          candidate.artifactCanonicalBytes,
        ) ||
        !equalCanonicalBytes(
          operation.choiceIdentity.canonicalBytes,
          candidate.choiceIdentityCanonicalBytes,
        ) ||
        operation.planBindingDigest !== candidate.targetBindingDigest ||
        operation.receiveIntent.plan.binding.stableName !== candidate.stablePhysicalName ||
        !sameDirectZipPolicies(operation.receiveIntent.plan.binding.policies, candidate.policies) ||
        !sameJournalPolicies(checkpoint.policies, candidate.policies) ||
        checkpoint.targetBindingDigest !== candidate.targetBindingDigest ||
        checkpoint.receiveIntentDigest !== operation.receiveIntentDigest ||
        checkpoint.entryOrdinal !== 0n || checkpoint.currentMember !== undefined ||
        checkpoint.layoutPages.pageCount !== 0n || checkpoint.centralPages.pageCount !== 0n ||
        checkpoint.epochPages.pageCount !== 0n ||
        lifecycle.operationId !== candidate.operationId ||
        lifecycle.receiveIntentDigest !== operation.receiveIntentDigest ||
        lifecycle.kind !== 'receiving' || lifecycle.activeLeaseId !== candidate.leaseId ||
        lease.operationId !== candidate.operationId || lease.leaseId !== candidate.leaseId ||
        candidate.leaseGeneration !== 1n ||
        handles.some(handle => handle.operationId !== candidate.operationId)) {
      throw new TypeError('Direct ZIP bootstrap commit does not promote one frozen operation')
    }
    const state = await createDirectZipStateRowV1(checkpoint, lease.leaseId)
    const transaction = this.#storage.database.transaction([
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite')
    try {
      const storedCandidate = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).get(candidate.id))
      if (!sameCandidateRow(storedCandidate, candidate)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP bootstrap candidate changed')
      }
      const storedLease = await this.#storage.requireLease(transaction, candidate.operationId)
      if (storedLease.leaseId !== candidate.leaseId) {
        throw new DirectZipJournalConcurrencyError()
      }
      const provisionalParent = await requestResult<unknown>(transaction
        .objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE).get(candidate.parentHandleId))
      if (!isHandleForOperation(provisionalParent, candidate.operationId, candidate.parentHandleId)) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP bootstrap parent locator changed')
      }
      const existingState = await requestResult(transaction
        .objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).get(state.id))
      if (existingState !== undefined) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP bootstrap state already exists')
      }
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).add(operationRecord)
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).add(lifecycleRecord)
      for (const handle of handles) {
        transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE).put(handle)
      }
      transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE).put(lease)
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).add(state)
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).delete(candidate.id)
      await transactionCompletion(transaction)
      this.#storage.emit({
        name: 'direct_zip.journal.bootstrap_committed',
        operation_id: candidate.operationId,
        lease_id: candidate.leaseId,
        checkpoint_generation: checkpoint.generation,
        candidate_id: candidate.candidateId,
      })
    } catch (error) {
      this.#storage.failed(candidate.operationId, candidate.leaseId, candidate.candidateId, error)
      abortQuietly(transaction)
      throw error
    }
  }


}
