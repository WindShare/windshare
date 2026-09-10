import {
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
  INDEXEDDB_DIRECT_ZIP_STATE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  requestResult,
  transactionCompletion,
} from '../../../browser/indexeddb-database'
import { validatePersistedReceiveRecord } from '../../../workspace/records'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../../../workspace/state-codec'
import type { DirectZipClosingTransitionV1 } from '../model'
import { createDirectZipStateRowV1, validateDirectZipCheckpointV1 } from '../records'
import {
  DirectZipJournalConcurrencyError,
  abortQuietly,
  assertLifecycleForCheckpoint,
  sameCheckpointResumeAuthority,
  samePersistedRecordRow,
  snapshotFence,
} from './authority'
import type { IndexedDbDirectZipJournalStorage } from './storage'

/** Closing changes replay intent without changing target bytes, so it has its own fenced cut. */
export async function enterDirectZipClosing(
  storage: IndexedDbDirectZipJournalStorage,
  cut: DirectZipClosingTransitionV1,
): Promise<void> {
  storage.assertOpen()
  const fence = snapshotFence(cut.fence)
  const checkpoint = await validateDirectZipCheckpointV1(cut.checkpoint)
  const expected = await storage.readFenceState(fence)
  const before = expected.checkpoint
  const { closingReplay, ...withoutClosing } = checkpoint
  if (before.phase !== 'between-members' || checkpoint.phase !== 'closing' ||
      checkpoint.generation !== before.generation + 1n ||
      checkpoint.predecessorCheckpointDigest !== before.digest ||
      checkpoint.candidateLineageDigest !== undefined ||
      checkpoint.targetObservation.digest !== before.targetObservation.digest ||
      closingReplay?.archiveOffset !== before.committedArchiveLength ||
      closingReplay.centralRecordRootDigest !== before.centralPages.rootDigest ||
      closingReplay.completion !== undefined ||
      !sameCheckpointResumeAuthority(before, {
        ...withoutClosing, phase: before.phase,
      })) {
    throw new TypeError('Direct ZIP closing transition changed committed content authority')
  }
  const lifecycleRecord = await validatePersistedReceiveRecord(cut.lifecycleRecord)
  const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
  if (!samePersistedRecordRow(await storedReceiveLifecycleState(cut.lifecycle), lifecycleRecord)) {
    throw new TypeError('Direct ZIP closing lifecycle projection changed')
  }
  assertLifecycleForCheckpoint(lifecycle, checkpoint, fence)
  const currentRecord = await storage.read<unknown>(INDEXEDDB_RECEIVE_RECORD_STORE, lifecycleRecord.id)
  if (currentRecord === undefined) throw new DirectZipJournalConcurrencyError('Direct ZIP lifecycle is absent')
  const current = await validatePersistedReceiveRecord(currentRecord as typeof lifecycleRecord)
  if (decodeStoredReceiveLifecycleState(current).generation + 1n !== lifecycle.generation) {
    throw new DirectZipJournalConcurrencyError('Direct ZIP closing lifecycle generation changed')
  }
  const state = await createDirectZipStateRowV1(checkpoint, fence.leaseId)
  const transaction = storage.database.transaction([
    INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE, INDEXEDDB_DIRECT_ZIP_STATE_STORE,
    INDEXEDDB_RECEIVE_LEASE_STORE, INDEXEDDB_RECEIVE_RECORD_STORE,
  ], 'readwrite')
  try {
    await storage.assertFence(transaction, fence, expected)
    const [count, storedLifecycle] = await Promise.all([
      requestResult(transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
        .index(INDEXEDDB_BY_OPERATION_INDEX).count(IDBKeyRange.only(fence.operationId))),
      requestResult<unknown>(transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).get(lifecycleRecord.id)),
    ])
    if (count !== 0 || !samePersistedRecordRow(storedLifecycle, current)) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP closing authority changed')
    }
    transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE).put(state)
    transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).put(lifecycleRecord)
    await transactionCompletion(transaction)
    storage.emit({
      name: 'direct_zip.journal.closing_entered', operation_id: fence.operationId,
      lease_id: fence.leaseId, checkpoint_generation: checkpoint.generation,
    })
  } catch (error) {
    storage.failed(fence.operationId, fence.leaseId, undefined, error)
    abortQuietly(transaction)
    throw error
  }
}
