/** Export readers outlive a backend session and may belong to another tab. */
export interface ArtifactReaderLease {
  release(): void
}

const localReaders = new Map<string, Set<Promise<void>>>()

export async function acquireArtifactReader(operationId: string): Promise<ArtifactReaderLease> {
  let release!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  const readers = localReaders.get(operationId) ?? new Set<Promise<void>>()
  readers.add(held)
  localReaders.set(operationId, readers)
  let released = false
  const lease = Object.freeze({
    release() {
      if (released) return
      released = true
      readers.delete(held)
      if (readers.size === 0) localReaders.delete(operationId)
      release()
    },
  })
  const locks = globalThis.navigator?.locks
  if (locks !== undefined) {
    try {
      await new Promise<void>((resolve, reject) => {
        locks.request(lockName(operationId), { mode: 'shared' }, async () => {
          resolve()
          await held
        }).catch(reject)
      })
    } catch (error) {
      lease.release()
      throw error
    }
  }
  return lease
}

export async function withArtifactCleanup<T>(
  operationId: string,
  cleanup: () => Promise<T>,
): Promise<T> {
  const locks = globalThis.navigator?.locks
  if (locks !== undefined) return locks.request(lockName(operationId), cleanup)
  // Deterministic non-browser adapters still retain live File readers.
  while (localReaders.has(operationId)) {
    await Promise.all(localReaders.get(operationId) ?? [])
  }
  return cleanup()
}

function lockName(operationId: string): string {
  return `windshare/artifact-readers/${operationId}`
}
