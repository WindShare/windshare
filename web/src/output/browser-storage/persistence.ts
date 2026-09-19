import { emitOutputTrace, outputTraceEvent, type OutputTraceEvent, type OutputTraceSource } from '../diagnostics/trace'

export type BrowserStoragePersistenceRuntime = Partial<Pick<StorageManager, 'persist'>>

const pendingRequests = new WeakMap<BrowserStoragePersistenceRuntime, string>()

type PersistenceTransition = OutputTraceEvent<'storage_persistence'>['payload']['transition']

/** Permission belongs to the origin and may wait indefinitely; no task owns or awaits it. */
export function requestBrowserStoragePersistence(
  storage: BrowserStoragePersistenceRuntime,
  operationId: string,
  trace?: OutputTraceSource,
): void {
  const observe = (transition: PersistenceTransition, requestOperationId = operationId): void => {
    emitOutputTrace(trace, () => outputTraceEvent('storage_persistence', {
      operation_id: operationId,
      request_operation_id: requestOperationId,
      transition,
    }))
  }
  const pendingOperationId = pendingRequests.get(storage)
  if (pendingOperationId !== undefined) {
    observe('already_pending', pendingOperationId)
    return
  }
  const settle = (transition: 'granted' | 'not_granted' | 'failed'): void => {
    pendingRequests.delete(storage)
    observe(transition)
  }
  try {
    const persist = storage.persist
    if (persist === undefined) {
      observe('unavailable')
      return
    }
    pendingRequests.set(storage, operationId)
    observe('requested')
    // Share only a pending request. Later tasks can observe a permission changed in browser settings.
    persist.call(storage).then(
      granted => settle(granted ? 'granted' : 'not_granted'),
      () => settle('failed'),
    )
  } catch {
    settle('failed')
  }
}
