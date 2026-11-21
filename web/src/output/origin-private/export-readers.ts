/** Export readers outlive a backend session and may belong to another tab. */
export interface ArtifactReaderLease {
  release(): void
}

const localReaders = new Map<string, Set<Promise<void>>>()
const localWriters = new Map<string, Promise<void>>()
const releasingLocks = new Map<string, Set<Promise<void>>>()

export async function acquireArtifactReader(operationId: string): Promise<ArtifactReaderLease> {
  const locks = globalThis.navigator?.locks
  let release!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  let completion: Promise<void> | undefined
  let registered = false
  let released = false
  const lease = Object.freeze({
    release() {
      if (released) return
      released = true
      if (completion !== undefined) trackReleasingLock(operationId, completion)
      if (registered) {
        const readers = localReaders.get(operationId)
        readers?.delete(held)
        if (readers !== undefined && readers.size === 0) localReaders.delete(operationId)
      }
      release()
    },
  })
  if (locks !== undefined) {
    try {
      await new Promise<void>((resolve, reject) => {
        const request = locks.request(lockName(operationId), { mode: 'shared' }, async () => {
          resolve()
          await held
        })
        completion = request.then(() => undefined, () => undefined)
        request.catch(reject)
      })
    } catch (error) {
      lease.release()
      throw error
    }
    return lease
  }
  while (localWriters.has(operationId)) {
    await localWriters.get(operationId)
  }
  const readers = localReaders.get(operationId) ?? new Set<Promise<void>>()
  readers.add(held)
  localReaders.set(operationId, readers)
  registered = true
  return lease
}

/**
 * Fails fast when a browser handoff is still reading slices from the mutable OPFS object,
 * preventing native writes from invalidating the browser's File snapshot mid-download.
 */
export async function acquireArtifactWriter(operationId: string): Promise<ArtifactReaderLease> {
  const locks = globalThis.navigator?.locks
  let release!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  let completion: Promise<void> | undefined
  let acquired = false
  let registered = false
  let released = false
  const lease = Object.freeze({
    release() {
      if (released) return
      released = true
      if (acquired && completion !== undefined) trackReleasingLock(operationId, completion)
      if (registered && localWriters.get(operationId) === held) localWriters.delete(operationId)
      release()
    },
  })
  if (locks !== undefined) {
    // Wait for any already-released same-realm lock to finish unwinding its Web Lock callback
    // before probing ifAvailable, without waiting on still-active readers.
    while (releasingLocks.has(operationId)) {
      await Promise.all(releasingLocks.get(operationId) ?? [])
    }
    try {
      await new Promise<void>((resolve, reject) => {
        const request = locks.request(lockName(operationId), { mode: 'exclusive', ifAvailable: true }, async lock => {
          if (lock === null) {
            reject(artifactWriterBusyError())
            return
          }
          acquired = true
          resolve()
          await held
        })
        completion = request.then(() => undefined, () => undefined)
        request.catch(reject)
      })
    } catch (error) {
      lease.release()
      throw error
    }
    return lease
  }
  if (localReaders.has(operationId) || localWriters.has(operationId)) {
    throw artifactWriterBusyError()
  }
  localWriters.set(operationId, held)
  registered = true
  return lease
}

function trackReleasingLock(operationId: string, completion: Promise<void>): void {
  const active = releasingLocks.get(operationId) ?? new Set<Promise<void>>()
  active.add(completion)
  releasingLocks.set(operationId, active)
  void completion.finally(() => {
    active.delete(completion)
    if (active.size === 0 && releasingLocks.get(operationId) === active) {
      releasingLocks.delete(operationId)
    }
  })
}

export async function withArtifactCleanup<T>(
  operationId: string,
  cleanup: () => Promise<T>,
): Promise<T> {
  const locks = globalThis.navigator?.locks
  if (locks !== undefined) return locks.request(lockName(operationId), cleanup)
  // Deterministic non-browser adapters still retain live File readers and writers.
  for (;;) {
    const activeWriter = localWriters.get(operationId)
    if (activeWriter !== undefined) {
      await activeWriter
      continue
    }
    const activeReaders = localReaders.get(operationId)
    if (activeReaders !== undefined) {
      await Promise.all(activeReaders)
      continue
    }
    break
  }
  return cleanup()
}

function artifactWriterBusyError(): DOMException {
  return new DOMException(
    'Partial download is still being handed off to the browser; wait briefly before continuing',
    'InvalidStateError',
  )
}

function lockName(operationId: string): string {
  return `windshare/artifact-readers/${operationId}`
}
