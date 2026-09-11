import {
  DEFAULT_OUTPUT_DATABASE_NAME, INDEXEDDB_BY_OPERATION_FILE_INDEX, INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  openIndexedDbCheckpointDatabase, requestResult, transactionCompletion,
} from '../browser/indexeddb-database'
import { readStoredCheckpoint } from '../browser/indexeddb/repository-transactions'
import { FILE_CHECKPOINT_COMMIT_VERIFIED } from '../persistence/checkpoint'
import { advanceBrowserDeliveryRecord } from './lifecycle'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from './model'
import { browserDeliveryStagingPath } from './records'
import type { BrowserDeliveryRepository } from './repository'
import { BrowserDeliveryResumeAccumulator, type BrowserDeliveryResumeSummary } from './retained'

const MAX_RECEIVING_CHECKPOINT_CANDIDATES = 2

/** A read-only candidate projection closes the crash gap before the delivery journal catches up. */
export async function readBrowserDeliveryResumeSummary(input: {
  readonly repository: BrowserDeliveryRepository
  readonly operationId: string
  readonly databaseName?: string
}): Promise<BrowserDeliveryResumeSummary | undefined> {
  const policy = await input.repository.readPolicy(input.operationId)
  if (policy === undefined) return undefined
  const accumulator = new BrowserDeliveryResumeAccumulator(policy)
  let database: IDBDatabase | undefined
  let cursor: string | undefined
  try {
    do {
      const page = await input.repository.scanFiles({
        operationId: policy.operationId, ...(cursor === undefined ? {} : { afterFileId: cursor }),
      })
      let projected = page.records
      if (page.records.some(record => record.placement === 'staged' && record.state.kind === 'receiving')) {
        database ??= await openIndexedDbCheckpointDatabase(input.databaseName ?? DEFAULT_OUTPUT_DATABASE_NAME)
        const authority = database
        projected = await Promise.all(page.records.map(record => projectReceivingCheckpoint(authority, policy, record)))
      }
      accumulator.add(projected)
      cursor = page.nextFileId
    } while (cursor !== undefined)
    return accumulator.summary()
  } finally { database?.close() }
}

async function projectReceivingCheckpoint(
  database: IDBDatabase,
  policy: BrowserSavePolicyV1,
  record: BrowserDeliveryRecordV1,
): Promise<BrowserDeliveryRecordV1> {
  if (record.placement !== 'staged' || record.state.kind !== 'receiving' || policy.staging === undefined) return record
  const transaction = database.transaction(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, 'readonly')
  const completion = transactionCompletion(transaction)
  completion.catch(() => undefined)
  const rows: unknown[] = await requestResult(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
    .index(INDEXEDDB_BY_OPERATION_FILE_INDEX).getAll(
      [policy.staging.operationId, record.fileId], MAX_RECEIVING_CHECKPOINT_CANDIDATES,
    ))
  await completion
  if (rows.length !== 1) return record
  const checkpoint = readStoredCheckpoint(rows[0])
  if (checkpoint.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
      checkpoint.fileRevision !== record.source.fileRevision || checkpoint.exactSize !== record.source.exactSize ||
      JSON.stringify(checkpoint.canonicalPath) !== JSON.stringify(browserDeliveryStagingPath(record.fileId))) return record
  // This temporary value is never written. The local engine must inspect ownership and commit the transition under its lease.
  return advanceBrowserDeliveryRecord(policy, record, { kind: 'receiving', checkpoint })
}
