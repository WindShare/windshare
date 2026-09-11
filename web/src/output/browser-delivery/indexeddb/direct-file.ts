import { INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_FILE_FINAL_PROOF_STORE } from '../../browser/indexeddb-database'
import type { IndexedDbFileCommitHost, IndexedDbFileCommitParticipant } from '../../browser/indexeddb/file-commit-participants'
import type { FileCheckpointV2 } from '../../persistence/checkpoint'
import type { PersistentFileTransactionPort } from '../../persistent-tree/contracts'
import type { BrowserDirectFileDelivery } from '../ports'
import type { BrowserDeliveryRecordV1 } from '../model'
import { createBrowserDeliveryFile, commitDirectBrowserDelivery, requirePolicy } from './file-transactions'
import { BrowserDeliveryConcurrencyError } from '../repository'

export async function openDirectDeliveryFile(
  host: IndexedDbFileCommitHost, delivery: BrowserDirectFileDelivery,
  open: () => Promise<PersistentFileTransactionPort>,
): Promise<PersistentFileTransactionPort> {
  const release = host.enlistFileCommit(delivery.currentRecord().fileId, directDeliveryParticipant(delivery))
  try {
    const transaction = await open()
    // Enrollment follows the writer's lifetime, including failed commits that can be retried.
    return {
      revision: transaction.revision, ownedObjectId: transaction.ownedObjectId,
      ...(transaction.checkpointPolicy === undefined ? {} : { checkpointPolicy: transaction.checkpointPolicy }),
      ...(transaction.checkpointObjectId === undefined ? {} : { checkpointObjectId: transaction.checkpointObjectId }),
      initialDurableRanges: transaction.initialDurableRanges,
      get verifiedRanges() { return transaction.verifiedRanges },
      writeRange: transaction.writeRange.bind(transaction),
      automaticCheckpoint: transaction.automaticCheckpoint.bind(transaction),
      checkpoint: transaction.checkpoint.bind(transaction),
      commit: async signal => { const result = await transaction.commit(signal); release(); return result },
      pause: async reason => { try { return await transaction.pause(reason) } finally { release() } },
      retire: async reason => { try { await transaction.retire(reason) } finally { release() } },
      close: async () => { try { await transaction.close() } finally { release() } },
    }
  } catch (error) { release(); throw error }
}

function directDeliveryParticipant(delivery: BrowserDirectFileDelivery): IndexedDbFileCommitParticipant {
  return {
    stores: [INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_FILE_FINAL_PROOF_STORE],
    apply: async (transaction, commit) => {
      const record = delivery.currentRecord()
      let next: BrowserDeliveryRecordV1
      if (commit.kind === 'initial-claim') {
        const policy = await requirePolicy(transaction, record.operationId)
        requireDirectCheckpoint(record, commit.checkpoint, policy.target)
        next = await createBrowserDeliveryFile(transaction, policy, record)
      } else {
        next = await commitDirectBrowserDelivery(transaction, record, commit.checkpoint, commit.finalProof)
      }
      return () => delivery.committed(next)
    },
  }
}

function requireDirectCheckpoint(record: BrowserDeliveryRecordV1, checkpoint: FileCheckpointV2,
  target: import('../model').BrowserSavePolicyV1['target']): void {
  if (record.placement !== 'direct' || checkpoint.operationId !== target.operationId ||
      checkpoint.receiveIntentDigest !== target.receiveIntentDigest || checkpoint.materializerKind !== target.materializerKind ||
      checkpoint.materializationBindingDigest !== target.materializationBindingDigest || checkpoint.authorityRef !== target.authorityRef ||
      checkpoint.fileId !== record.fileId || checkpoint.fileRevision !== record.source.fileRevision || checkpoint.exactSize !== record.source.exactSize ||
      JSON.stringify(checkpoint.canonicalPath) !== JSON.stringify(record.materializationRelativePath)) {
    throw new BrowserDeliveryConcurrencyError('Direct delivery differs from its checkpoint authority')
  }
}
