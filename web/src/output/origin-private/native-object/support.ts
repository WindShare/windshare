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
export function probeNativeObjectSupport(runtime: NativeSupportRuntime): Promise<boolean> {
  const WorkerConstructor = runtime.Worker
  if (WorkerConstructor === undefined) return Promise.resolve(false)
  return new Promise(resolve => {
    let worker: NativeSupportWorker
    try {
      worker = new WorkerConstructor(new URL('./support-worker.ts', import.meta.url), { type: 'module' })
    } catch {
      resolve(false)
      return
    }
    let settled = false
    const finish = (supported: boolean) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      worker.terminate()
      resolve(supported)
    }
    const timer = setTimeout(() => finish(false), NATIVE_SUPPORT_PROBE_TIMEOUT_MS)
    worker.onmessage = event => finish(event.data === true)
    worker.onerror = () => finish(false)
    worker.onmessageerror = () => finish(false)
  })
}
