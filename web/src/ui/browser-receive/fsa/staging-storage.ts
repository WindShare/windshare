import type { BrowserStagingStorageFacts } from '../../../output/planning/staging-storage'

export type BrowserStagingStorageRuntime = Partial<Pick<StorageManager, 'estimate' | 'persisted'>>

/** Browser storage facts are independent of the destination disk and download handoff. */
export async function inspectBrowserStagingStorage(
  storage: BrowserStagingStorageRuntime | undefined,
  opfsUsable: boolean,
  signal?: AbortSignal,
): Promise<BrowserStagingStorageFacts> {
  signal?.throwIfAborted()
  const [quota, persistence] = await Promise.all([
    readQuota(storage),
    readPersistence(storage),
  ])
  signal?.throwIfAborted()
  return Object.freeze({
    opfs: opfsUsable ? 'usable' : 'unavailable',
    persistence,
    quota,
    pressure: 'normal',
  })
}

async function readQuota(storage: BrowserStagingStorageRuntime | undefined): Promise<BrowserStagingStorageFacts['quota']> {
  try {
    const estimate = await storage?.estimate?.()
    if (estimate === undefined || !validBytes(estimate.quota) || !validBytes(estimate.usage)) {
      return Object.freeze({ kind: 'unknown' })
    }
    return Object.freeze({
      kind: 'estimated',
      usageBytes: BigInt(estimate.usage),
      quotaBytes: BigInt(estimate.quota),
    })
  } catch {
    // Failure to observe quota cannot revoke an otherwise usable storage backend.
    return Object.freeze({ kind: 'unknown' })
  }
}

async function readPersistence(storage: BrowserStagingStorageRuntime | undefined): Promise<BrowserStagingStorageFacts['persistence']> {
  try {
    const persisted = await storage?.persisted?.()
    if (persisted === undefined) return 'unknown'
    return persisted ? 'persisted' : 'not-persisted'
  } catch {
    return 'unknown'
  }
}

function validBytes(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}
