export const TRACE_ACTIVATION_STORAGE_KEY = 'windshare.diagnostics.capture-expires-at'

export interface TraceActivationStore {
  readExpiry(): number | undefined
  writeExpiry(expiresAtMilliseconds: number): void
  clear(): void
}

type TraceActivationStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>

export function createBrowserTraceActivationStore(
  storage: () => TraceActivationStorage,
  pageUrl: string,
): TraceActivationStore {
  const scope = new URL(pageUrl).pathname
  const key = `${TRACE_ACTIVATION_STORAGE_KEY}:${scope}`
  // Each share and tab owns its deadline; storage denial must remain optional.
  return Object.freeze({
    readExpiry: () => {
      const value = storage().getItem(key)
      return value === null ? undefined : Number(value)
    },
    writeExpiry: (expiresAtMilliseconds: number) => {
      storage().setItem(key, String(expiresAtMilliseconds))
    },
    clear: () => storage().removeItem(key),
  })
}
