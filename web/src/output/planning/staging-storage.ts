export interface BrowserStagingStorageFacts {
  readonly opfs: 'usable' | 'unavailable'
  readonly persistence: 'persisted' | 'not-persisted' | 'unknown'
  readonly quota:
    | Readonly<{ kind: 'estimated'; usageBytes: bigint; quotaBytes: bigint }>
    | Readonly<{ kind: 'unknown' }>
  readonly pressure: 'normal' | 'drain-first'
}

/** Missing estimates and eviction permission cannot revoke an otherwise usable OPFS writer. */
export function browserStagingQuota(
  estimate: Readonly<{ usage?: number; quota?: number }> | undefined,
): BrowserStagingStorageFacts['quota'] {
  const usage = estimate?.usage
  const quota = estimate?.quota
  if (usage === undefined || quota === undefined ||
      !Number.isSafeInteger(usage) || !Number.isSafeInteger(quota) || usage < 0 || quota < 0) {
    return Object.freeze({ kind: 'unknown' })
  }
  return Object.freeze({ kind: 'estimated', usageBytes: BigInt(usage), quotaBytes: BigInt(quota) })
}
