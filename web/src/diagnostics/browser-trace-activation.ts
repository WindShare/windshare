import type { TraceActivationStore } from './trace/switch'

export const TRACE_ACTIVATION_STORAGE_KEY = 'windshare.diagnostics.capture-expires-at'

type TraceActivationStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>

export function createBrowserTraceActivationStore(
  storage: () => TraceActivationStorage,
): TraceActivationStore {
  // Resolve storage lazily: browsers may deny even the localStorage getter.
  return Object.freeze({
    readExpiry: () => {
      const value = storage().getItem(TRACE_ACTIVATION_STORAGE_KEY)
      return value === null ? undefined : Number(value)
    },
    writeExpiry: (expiresAtMilliseconds: number) => {
      storage().setItem(TRACE_ACTIVATION_STORAGE_KEY, String(expiresAtMilliseconds))
    },
    clear: () => storage().removeItem(TRACE_ACTIVATION_STORAGE_KEY),
  })
}
