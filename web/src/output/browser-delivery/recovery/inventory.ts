import { IndexedDbReceiveOperationRepository } from '../../browser/indexeddb/receive-operation-repository'
import { acquireBrowserReceiveOperationLease, BrowserReceiveOperationBusyError } from '../../browser/session-lease'
import type { ReceiveLifecycleState } from '../../workspace/state'
import { scanAllFSAFileCheckpoints } from '../../file-system-access/checkpoint-repository'
import type { DirectTreeIntent } from '../../file-system-access/settlement-proof'
import type { FileCheckpointJournal } from '../../persistence/journal'
import { createFSARecoveryCheckpointSnapshot, deriveFSARecoverySummary, type RecoverySummary } from '../../file-system-access/recovery-summary'
import { IndexedDbBrowserDeliveryRepository } from '../indexeddb'
import type { BrowserDeliveryRecordV1 } from '../model'
import { deriveBrowserDeliveryLifecycle, type LocalFileSetLifecycle } from './authority'
import { reconcileBrowserDeliveryLifecycle } from './local-lifecycle'

export async function repairBrowserDeliveryInventoryLifecycle(
  lifecycle: ReceiveLifecycleState, databaseName: string,
): Promise<ReceiveLifecycleState> {
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set') return lifecycle
  const delivery = await readDelivery(lifecycle.operationId, databaseName)
  if (delivery === undefined || !delivery.records.some(record =>
    record.localMutation?.lifecycleGeneration === lifecycle.generation)) return lifecycle
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    let lease
    try { lease = await acquireBrowserReceiveOperationLease(repository, lifecycle.operationId) }
    catch (error) {
      if (error instanceof BrowserReceiveOperationBusyError) return lifecycle
      throw error
    }
    try { return await reconcileBrowserDeliveryLifecycle({ repository, lease, databaseName }) }
    finally { await lease.release() }
  } finally { repository.close() }
}

/** A live copy may commit immediately after the scan; authority and costs must therefore use the same immutable cut. */
export async function readBrowserDeliveryRecoverySummary(input: {
  lifecycle: LocalFileSetLifecycle; intent: DirectTreeIntent; checkpoints: FileCheckpointJournal; databaseName: string
}): Promise<RecoverySummary | undefined> {
  const delivery = await readDelivery(input.lifecycle.operationId, input.databaseName)
  const checkpoints = await scanAllFSAFileCheckpoints(input.checkpoints, 'committed')
  const snapshot = await createFSARecoveryCheckpointSnapshot(input.intent, input.lifecycle.generation, checkpoints)
  if (delivery !== undefined) {
    const next = await deriveBrowserDeliveryLifecycle({ ...input, ...delivery, checkpoints: snapshot.checkpoints })
    if (next.generation !== input.lifecycle.generation) return undefined
  }
  return deriveFSARecoverySummary({ intent: input.intent, lifecycle: input.lifecycle, snapshot })
}

async function readDelivery(operationId: string, databaseName: string) {
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName })
  try {
    const policy = await journal.readPolicy(operationId)
    if (policy === undefined) return undefined
    const records: BrowserDeliveryRecordV1[] = []
    let afterFileId: string | undefined
    do {
      const page = await journal.scanFiles({ operationId, ...(afterFileId === undefined ? {} : { afterFileId }) })
      records.push(...page.records)
      afterFileId = page.nextFileId
    } while (afterFileId !== undefined)
    return { policy, records }
  } finally { journal.close() }
}
