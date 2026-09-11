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

export function operationDeadlineSignal(
  parent: AbortSignal,
  remainingMilliseconds: number,
  timeoutReason: unknown,
): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent.reason ?? new DOMException('Revision lease renewal aborted', 'AbortError'),
  )
  parent.addEventListener('abort', abort, { once: true })
  if (parent.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(timeoutReason)
  }, Math.max(0, Math.ceil(remainingMilliseconds)))
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent.removeEventListener('abort', abort)
    },
  }
}

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
