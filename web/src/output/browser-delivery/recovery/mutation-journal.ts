import {
  DEFAULT_OUTPUT_DATABASE_NAME, INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_BY_OPERATION_FILE_INDEX,
  INDEXEDDB_RECEIVE_LEASE_STORE, INDEXEDDB_RECEIVE_RECORD_STORE,
  openIndexedDbCheckpointDatabase, requestResult, transactionCompletion,
} from '../../browser/indexeddb-database'
import { readStoredCheckpoint } from '../../browser/indexeddb/repository-transactions'
import { fileCheckpointDigest } from '../../persistence/checkpoint'
import {
  receiveOperationLeaseId, validateReceiveOperationLeaseRecord,
  type PersistedReceiveRecord, type ReceiveOperationLeaseRecord,
} from '../../workspace/records'
import { decodeStoredReceiveLifecycleState } from '../../workspace/state-codec'
import { localMutationFields, snapshotBrowserDeliveryRecord, validateBrowserDeliveryRecord } from '../records'
import { validateBrowserSavePolicy } from '../policy'
import { BrowserDeliveryConcurrencyError } from '../repository'
import type { BrowserDeliveryRecordV1, BrowserDeliveryLocalMutation, BrowserSavePolicyV1 } from '../model'

export async function persistBrowserDeliveryLocalMutation(input: {
  databaseName?: string
  previous: BrowserDeliveryRecordV1
  mutation: BrowserDeliveryLocalMutation
  lifecycleRecord: PersistedReceiveRecord
  leaseId: string
}): Promise<void> {
  const database = await openIndexedDbCheckpointDatabase(input.databaseName ?? DEFAULT_OUTPUT_DATABASE_NAME)
  const transaction = database.transaction([
    INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
    INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_RECEIVE_LEASE_STORE, INDEXEDDB_RECEIVE_RECORD_STORE,
  ], 'readwrite')
  const completion = transactionCompletion(transaction)
  completion.catch(() => undefined)
  try {
    await persistMutation(transaction, input)
    await completion
  } catch (error) {
    try { transaction.abort() } catch { /* A failed request may have already ended the transaction. */ }
    await completion.catch(() => undefined)
    throw error
  } finally { database.close() }
}

async function persistMutation(transaction: IDBTransaction, input: {
  previous: BrowserDeliveryRecordV1; mutation: BrowserDeliveryLocalMutation
  lifecycleRecord: PersistedReceiveRecord; leaseId: string
}): Promise<void> {
  const { previous, mutation } = input
  const store = transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
  const [rawPolicy, rawCurrent, rawLifecycle, rawLease, rawCheckpoints] = await Promise.all([
    requestResult<BrowserSavePolicyV1>(transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE).get(previous.operationId)),
    requestResult<BrowserDeliveryRecordV1>(store.get([previous.operationId, previous.fileId])),
    requestResult<PersistedReceiveRecord>(transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE).get(input.lifecycleRecord.id)),
    requestResult<ReceiveOperationLeaseRecord>(transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE)
      .get(receiveOperationLeaseId(previous.operationId))),
    requestResult<unknown[]>(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
      .index(INDEXEDDB_BY_OPERATION_FILE_INDEX).getAll([previous.operationId, previous.fileId], 2)),
  ])
  if (rawPolicy === undefined || rawCurrent === undefined || rawLifecycle === undefined || rawLease === undefined ||
      rawLifecycle.digest !== input.lifecycleRecord.digest ||
      validateReceiveOperationLeaseRecord(rawLease).leaseId !== input.leaseId) {
    throw new BrowserDeliveryConcurrencyError('Local delivery requires the exact lifecycle and operation lease')
  }
  const lifecycle = decodeStoredReceiveLifecycleState(rawLifecycle)
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set' ||
      lifecycle.generation !== mutation.lifecycleGeneration || lifecycle.checkpointSetDigest !== mutation.checkpointSetDigest) {
    throw new BrowserDeliveryConcurrencyError('Local delivery baseline no longer matches the lifecycle')
  }
  const policy = validateBrowserSavePolicy(rawPolicy)
  const current = validateBrowserDeliveryRecord(policy, rawCurrent)
  if (current.digest !== previous.digest) throw new BrowserDeliveryConcurrencyError()
  if (JSON.stringify(localMutationFields(current.localMutation)) === JSON.stringify(localMutationFields(mutation))) return
  if (current.localMutation !== undefined && current.localMutation.lifecycleGeneration >= mutation.lifecycleGeneration) {
    throw new BrowserDeliveryConcurrencyError('Local delivery cannot replace an unreconciled baseline')
  }
  const checkpoints = rawCheckpoints.map(readStoredCheckpoint)
  const prior = mutation.priorTargetCheckpoint
  if (checkpoints.length !== (prior === undefined ? 0 : 1) ||
      (prior !== undefined && fileCheckpointDigest(checkpoints[0]!) !== fileCheckpointDigest(prior))) {
    throw new BrowserDeliveryConcurrencyError('Local delivery baseline is not the exact committed target checkpoint')
  }
  await requestResult(store.put(snapshotBrowserDeliveryRecord(policy, {
    ...current, generation: current.generation + 1n, localMutation: mutation,
  })))
}
