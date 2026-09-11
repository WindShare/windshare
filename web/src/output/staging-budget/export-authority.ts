export const STAGING_EXPORT_LOCK_NAME = 'windshare/browser-delivery/local-copy/v1'
declare const stagingExportAuthorityBrand: unique symbol

export interface StagingExportAuthority {
  readonly [stagingExportAuthorityBrand]: true
  readonly ownerId: string
  readonly scope: 'origin' | 'context'
}

const activeAuthorities = new WeakSet<object>()
let localCopyTail: Promise<unknown> = Promise.resolve()

/** The callback lifetime, not a durable file phase or a timer, proves exclusive exporter ownership. */
export function withStagingExportAuthority<T>(
  work: (authority: StagingExportAuthority) => Promise<T>, signal?: AbortSignal,
): Promise<T> {
  const next = localCopyTail.catch(() => undefined).then(async () => {
    signal?.throwIfAborted()
    const locks = globalThis.navigator?.locks
    const run = async (scope: StagingExportAuthority['scope']) => {
      const authority = Object.freeze({ ownerId: crypto.randomUUID(), scope }) as StagingExportAuthority
      activeAuthorities.add(authority)
      try { return await work(authority) } finally { activeAuthorities.delete(authority) }
    }
    if (locks === undefined) return run('context')
    return locks.request(STAGING_EXPORT_LOCK_NAME, signal === undefined ? {} : { signal },
      () => run('origin'))
  })
  localCopyTail = next
  return next
}

export function assertStagingExportAuthority(
  authority: StagingExportAuthority, scope: 'origin' | 'context',
): void {
  if (!activeAuthorities.has(authority) || (scope === 'origin' && authority.scope !== 'origin')) {
    throw new DOMException('Staging export requires a current exclusive site authority', 'InvalidStateError')
  }
}
