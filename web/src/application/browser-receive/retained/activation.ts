import { IndexedDbReceiveOperationRepository } from '../../../output/browser/indexeddb-repository'
import { decodeStoredReceiveOperation, operationRecordId, RECEIVE_RECORD_OPERATION } from '../../../output/workspace/records'
import type { RetainedReceiveAction, RetainedReceiveOperation } from '../../receive/contracts'

export type RetainedActivation = (action: RetainedReceiveAction) => Promise<void>
const TARGET_ACTIONS: ReadonlySet<RetainedReceiveAction> = new Set(['continue', 'redownload', 'save-staged-files', 'discard', 'delete'])
const READ_WRITE = Object.freeze({ mode: 'readwrite' as const })
type PermissionHandle = FileSystemDirectoryHandle & {
  requestPermission?(options: typeof READ_WRITE): Promise<PermissionState>
}

/** Preparation holds a permission handle, never a lease or permission to mutate the saved task. */
export async function prepareRetainedActivation(operation: RetainedReceiveOperation, signal: AbortSignal): Promise<RetainedActivation | undefined> {
  if (!operation.actions.some(action => TARGET_ACTIONS.has(action))) return undefined
  signal.throwIfAborted()
  const repository = await IndexedDbReceiveOperationRepository.open()
  try {
    const record = await repository.readRecord(operationRecordId(operation.operationId, RECEIVE_RECORD_OPERATION))
    if (record === undefined) return undefined
    const stored = await decodeStoredReceiveOperation(record)
    if (stored.operationId !== operation.operationId || stored.receiveIntentDigest !== operation.receiveIntentDigest) {
      throw new DOMException('Retained authorization belongs to another download', 'InvalidStateError')
    }
    if (stored.receiveIntent.plan.kind !== 'direct-tree') return undefined
    const { verifyFSAOperationBinding } = await import('../../../output/browser/indexeddb-root-binding')
    const parent: PermissionHandle = (await verifyFSAOperationBinding({ repository, intent: stored.receiveIntent })).parent
    signal.throwIfAborted()
    return action => {
      // The native request starts in the click stack. Reopen still verifies the
      // persisted identity and acquires the mutation lease after permission resolves.
      if (!TARGET_ACTIONS.has(action) || parent.requestPermission === undefined) return Promise.resolve()
      return parent.requestPermission(READ_WRITE).then(permission => {
        if (permission !== 'granted') throw new DOMException('Access to the saved destination is required', 'NotAllowedError')
      })
    }
  } finally { repository.close() }
}
