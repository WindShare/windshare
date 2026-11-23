export const NATIVE_SUPPORT_PROBE_TIMEOUT_MS = 5_000

export interface NativeSupportWorker {
  onmessage: ((event: MessageEvent<unknown>) => void) | null
  onerror: ((event: ErrorEvent) => void) | null
  onmessageerror: ((event: MessageEvent<unknown>) => void) | null
  terminate(): void
}

export interface NativeSupportRuntime {
  readonly Worker?: new (url: URL, options: WorkerOptions) => NativeSupportWorker
}

/** Window API presence cannot establish a Dedicated Worker's sync-access capability. */
export function probeNativeObjectSupport(
  runtime: NativeSupportRuntime,
  signal?: AbortSignal,
): Promise<boolean> {
  if (signal?.aborted) return Promise.reject(abortReason(signal))
  // Vite recognizes Worker entries by this constructor name and the direct URL expression.
  // Keep the injected constructor under that name so production emits compiled, same-origin JS.
  const Worker = runtime.Worker
  if (Worker === undefined) return Promise.resolve(false)
  return new Promise((resolve, reject) => {
    let worker: NativeSupportWorker
    try {
      worker = new Worker(new URL('./support-worker.ts', import.meta.url), { type: 'module' })
    } catch {
      resolve(false)
      return
    }
    if (signal?.aborted) {
      worker.terminate()
      reject(abortReason(signal))
      return
    }
    let settled = false
    const onAbort = () => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      worker.terminate()
      reject(abortReason(signal))
    }
    const finish = (supported: boolean) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      signal?.removeEventListener('abort', onAbort)
      worker.terminate()
      resolve(supported)
    }
    const timer = setTimeout(() => finish(false), NATIVE_SUPPORT_PROBE_TIMEOUT_MS)
    signal?.addEventListener('abort', onAbort, { once: true })
    worker.onmessage = event => finish(event.data === true)
    worker.onerror = () => finish(false)
    worker.onmessageerror = () => finish(false)
  })
}

function abortReason(signal: AbortSignal | undefined): unknown {
  return signal?.reason ?? new DOMException('Native support probe aborted', 'AbortError')
}
