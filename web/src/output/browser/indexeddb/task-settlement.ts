import { snapshotTaskCheckpoint, type TaskCheckpoint } from '../../origin-private/task-checkpoint/model'
import { INDEXEDDB_OPFS_TASK_STORE, requestResult } from '../indexeddb-database'

/** Filesystem flush precedes this transaction; a concurrent writer invalidates the entire lifecycle cut. */
export async function assertWorkspaceCheckpoint(transaction: IDBTransaction, expected: TaskCheckpoint): Promise<void> {
  const key = JSON.stringify([expected.object.operationId, expected.object.objectId])
  const stored = await requestResult<{ checkpoint: TaskCheckpoint } | undefined>(
    transaction.objectStore(INDEXEDDB_OPFS_TASK_STORE).get(key),
  )
  if (stored === undefined || checkpointValue(snapshotTaskCheckpoint(stored.checkpoint)) !== checkpointValue(expected)) {
    throw new DOMException('Archive checkpoint changed before lifecycle settlement', 'InvalidStateError')
  }
}

function checkpointValue(checkpoint: TaskCheckpoint): string {
  return JSON.stringify(checkpoint, (_, value: unknown) => typeof value === 'bigint' ? value.toString() : value)
}
