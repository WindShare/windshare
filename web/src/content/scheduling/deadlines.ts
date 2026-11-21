export function linkAbortSignals(...sources: readonly AbortSignal[]): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const listeners = sources.map((source) => {
    const abort = () => controller.abort(
      source.reason ?? new DOMException('Content operation aborted', 'AbortError'),
    )
    source.addEventListener('abort', abort, { once: true })
    if (source.aborted) abort()
    return { source, abort }
  })
  return {
    signal: controller.signal,
    close: () => {
      for (const { source, abort } of listeners) source.removeEventListener('abort', abort)
    },
  }
}

export { operationDeadlineSignal } from '../../contracts/deadline'

export function delayWithAbort(milliseconds: number, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  return new Promise((resolve, reject) => {
    const timer = globalThis.setTimeout(() => {
      signal.removeEventListener('abort', abort)
      resolve()
    }, Math.max(0, Math.ceil(milliseconds)))
    const abort = () => {
      globalThis.clearTimeout(timer)
      reject(signal.reason ?? new DOMException('Lease renewal retry aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
  })
}
