import { IndexedDbReceiveOperationRepository } from '../../../output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease, type BrowserReceiveOperationLease } from '../../../output/browser/session-lease'
import { openBrowserFolderDelivery, openBrowserFolderDeliveryCleanup } from '../../../output/browser-delivery/assembly'
import { beginBrowserDeliveryLocalMutation, reconcileBrowserDeliveryLifecycle } from '../../../output/browser-delivery/recovery/local-lifecycle'
import { isBrowserDeliveryLocalLifecycle, type BrowserDeliveryLocalAction } from '../../../output/browser-delivery/recovery/local-actions'
import { isTerminalLifecycleState, type ReceiveLifecycleState } from '../../../output/workspace/state'
import { IndexedDbBrowserDeliveryRepository } from '../../../output/browser-delivery/indexeddb'
import { reopenFileSystemAccessOutput, type FileSystemAccessOutputSession } from '../../../output/file-system-access/session'
import { decodeStoredReceiveOperation, operationRecordId, RECEIVE_RECORD_OPERATION } from '../../../output/workspace/records'
import { decodeStoredReceiveLifecycleState } from '../../../output/workspace/state-codec'
import type { ReceiveOperationRepository } from '../../../output/workspace/repository'
import type { OutputDiagnosticsPorts } from '../../../output/diagnostics'
import type { BrowserReceiveWindow } from '../contracts'
import type { V2RetainedReceiveOperation } from '../../v2-receive-runtime'
import { inspectBrowserStagingStorage } from './staging-storage'
import { browserFolderDeliveryTrace } from './delivery-trace'

type LocalFolderReference = Pick<V2RetainedReceiveOperation, 'operationId' | 'receiveIntentDigest' | 'lifecycleGeneration'>

/** Local delivery takes fresh storage authority without starting a receive lifecycle or contacting the sender. */
export async function runRetainedBrowserFolderAction(
  windowPort: BrowserReceiveWindow,
  reference: LocalFolderReference,
  action: BrowserDeliveryLocalAction | 'discard-staging',
  signal: AbortSignal,
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  signal.throwIfAborted()
  const repository = await IndexedDbReceiveOperationRepository.open()
  let lease: BrowserReceiveOperationLease | undefined
  let target: FileSystemAccessOutputSession | undefined
  let delivery: Awaited<ReturnType<typeof openBrowserFolderDelivery | typeof openBrowserFolderDeliveryCleanup>> | undefined
  let localWorkAuthorized = false
  const failures: unknown[] = []
  try {
    lease = await acquireBrowserReceiveOperationLease(repository, reference.operationId, {
      manager: windowPort.navigator.locks,
    })
    const { intent, policy, lifecycle } = await readLocalFolderAuthority(repository, reference)
    requireLocalFolderActionLifecycle(action, lifecycle)
    localWorkAuthorized = true
    signal.throwIfAborted()
    const deliveryInput = {
      intent, operationLease: lease,
      preference: policy.preference, storage: windowPort.navigator.storage,
      storageFacts: () => inspectBrowserStagingStorage(windowPort.navigator.storage, true, signal),
      trace: browserFolderDeliveryTrace(diagnostics),
      ...(diagnostics === undefined ? {} : { diagnostics }),
    }
    if (action === 'save-staged-files') {
      await beginBrowserDeliveryLocalMutation({ repository, lease })
      target = await reopenFileSystemAccessOutput({ intent, operationRepository: repository,
        ...(diagnostics === undefined ? {} : { diagnostics }) })
      await target.activate()
      const receivingDelivery = await openBrowserFolderDelivery({ ...deliveryInput, target })
      delivery = receivingDelivery
      await receivingDelivery.saveStagedFiles(signal)
    } else {
      delivery = await openBrowserFolderDeliveryCleanup(deliveryInput)
      if (action === 'cleanup-staging') await delivery.cleanupStaging(signal)
      else if (action === 'discard-incomplete-staging') await delivery.discardIncompleteStaging(signal)
      else await delivery.discardStaging(signal)
    }
  } catch (error) {
    failures.push(error)
  }
  // Release all owners while keeping the first failure as the diagnostic cause.
  for (const close of [
    () => delivery?.close(),
    () => target?.closeForTerminalSettlement(),
    () => lease !== undefined && localWorkAuthorized
      ? reconcileBrowserDeliveryLifecycle({ repository, lease }) : undefined,
    () => lease?.release(),
    () => repository.close(),
    () => target?.releaseRootLease(),
  ]) {
    try { await close() } catch (error) { failures.push(error) }
  }
  if (failures.length === 1) throw failures[0]
  if (failures.length > 1) throw new AggregateError(failures,
    'Local folder saving failed while releasing storage authority', { cause: failures[0] })
}

function requireLocalFolderActionLifecycle(action: BrowserDeliveryLocalAction | 'discard-staging', lifecycle: ReceiveLifecycleState): void {
  if (action === 'discard-staging') return
  if (!isBrowserDeliveryLocalLifecycle(lifecycle) ||
      (action === 'discard-incomplete-staging' && !isTerminalLifecycleState(lifecycle))) {
    throw new DOMException('Local folder action does not own this receive disposition', 'InvalidStateError')
  }
}

async function readLocalFolderAuthority(repository: ReceiveOperationRepository, reference: LocalFolderReference) {
  const [record, lifecycleRecord] = await Promise.all([
    repository.readRecord(operationRecordId(reference.operationId, RECEIVE_RECORD_OPERATION)),
    repository.readLifecycle(reference.operationId),
  ])
  if (record === undefined || lifecycleRecord === undefined) throw new TypeError('Retained folder operation is missing')
  const operation = await decodeStoredReceiveOperation(record)
  const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
  if (operation.receiveIntentDigest !== reference.receiveIntentDigest ||
      lifecycle.operationId !== reference.operationId || lifecycle.receiveIntentDigest !== reference.receiveIntentDigest ||
      lifecycle.generation !== reference.lifecycleGeneration || operation.receiveIntent.plan.kind !== 'direct-tree') {
    throw new DOMException('Retained folder operation changed before local saving', 'InvalidStateError')
  }
  const journal = await IndexedDbBrowserDeliveryRepository.open()
  const policy = await journal.readPolicy(reference.operationId).finally(() => journal.close())
  if (policy === undefined || policy.receiveIntentDigest !== reference.receiveIntentDigest) {
    throw new TypeError('Retained folder operation has no matching delivery policy')
  }
  return { intent: operation.receiveIntent, policy, lifecycle }
}
