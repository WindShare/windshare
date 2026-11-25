import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { describe, expect, it, vi } from 'vitest'

import {
  CatalogClient,
  CatalogClientError,
  withCatalogLoadLock,
} from '../../src/catalog/client'
import {
  MemoryCatalogPageStore,
  CatalogPageStoreError,
  type CatalogPageStore,
  type CommittedDirectory,
} from '../../src/catalog/page-store'
import {
  PATH_POLICY,
  type CatalogPage,
  type ShareDescriptor,
} from '../../src/catalog/records'
import { contentPayload } from '../protocol/delivery-vectors'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'
import { createSignedRootCollisionFixture } from './signed-root-collision-fixture'
import { runSignedCatalogOwnershipProbe } from './signed-catalog-ownership-probe'

interface IdentityVector extends VectorCase {

  readonly senderPublicKeyB64: string
  readonly shareInstanceB64: string
}

function bytes(encoded: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(b64ToBytes(encoded))
}

describe('v2 catalog multi-receiver ownership', () => {
  it('reports cross-page and cross-directory ownership collisions as protocol failures', async () => {
    await expect(runSignedCatalogOwnershipProbe(new MemoryCatalogPageStore())).resolves.toEqual({
      crossPageNameRejected: true,
      crossPageNodeRejected: true,
      crossDirectoryNodeRejected: true,
      everyCollisionReportedAsProtocol: true,
      firstDirectoryCommitSurvived: true,
    })
  })

  it('rejects an authenticated page that reintroduces the synthetic root as a child', async () => {
    const fixture = await createSignedRootCollisionFixture()
    const store = new MemoryCatalogPageStore()
    const protocolFailures: unknown[] = []
    const client = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: {
        fetchPage: async () => fixture.catalogObject,
        failProtocol: async (reason) => { protocolFailures.push(reason) },
      },
      store,
    })
    try {
      await expect(client.loadDirectory(fixture.descriptor.syntheticRoot))
        .rejects.toBeInstanceOf(CatalogClientError)
      expect(protocolFailures).toHaveLength(1)
      expect(protocolFailures[0]).toMatchObject({
        message: 'Catalog entry reuses the synthetic-root identity',
      })
      await expect(store.loadDirectory(fixture.descriptor.syntheticRootId))
        .resolves.toBeUndefined()
    } finally {
      client.close()
      fixture.close()
    }
  })

  it('cleans up a late fetch after the last waiter cancels without staging or committing', async () => {
    const fixture = await createSignedRootCollisionFixture()
    const fetchStarted = deferred<void>()
    const lateFetch = deferred<Uint8Array<ArrayBuffer>>()
    const store = new ObservedCatalogStore()
    const client = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: {
        fetchPage: async () => {
          fetchStarted.resolve()
          return lateFetch.promise
        },
      },
      store,
    })
    const cancellation = new AbortController()
    const load = client.loadDirectory(fixture.descriptor.syntheticRoot, {
      signal: cancellation.signal,
    })
    await fetchStarted.promise
    cancellation.abort(new DOMException('receiver left', 'AbortError'))
    await expect(load).rejects.toMatchObject({ name: 'AbortError' })

    lateFetch.resolve(fixture.catalogObject)
    await store.aborted.promise
    expect(store.stages).toBe(0)
    expect(store.commits).toBe(0)
    client.close()
    fixture.close()
  })

  it('keeps storage open until a late fetch observes client close and removes staging', async () => {
    const fixture = await createSignedRootCollisionFixture()
    const fetchStarted = deferred<void>()
    const lateFetch = deferred<Uint8Array<ArrayBuffer>>()
    const store = new ObservedCatalogStore()
    const client = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: {
        fetchPage: async () => {
          fetchStarted.resolve()
          return lateFetch.promise
        },
      },
      store,
    })
    const load = client.loadDirectory(fixture.descriptor.syntheticRoot)
    await fetchStarted.promise
    client.close()
    expect(store.closes).toBe(0)

    lateFetch.resolve(fixture.catalogObject)
    await expect(load).rejects.toMatchObject({ name: 'AbortError' })
    await store.closed.promise
    expect(store.stages).toBe(0)
    expect(store.commits).toBe(0)
    expect(store.aborts).toBe(1)
    fixture.close()
  })

  it('aborts staged authority when cancellation arrives while stage persistence is pending', async () => {
    const fixture = catalogVectorFixture()
    const stageGate = deferred<void>()
    const store = new ObservedCatalogStore(stageGate.promise)
    const client = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: { fetchPage: async () => fixture.catalogPayload },
      store,
    })
    const cancellation = new AbortController()
    const load = client.loadDirectory(fixture.directoryId, { signal: cancellation.signal })
    await store.staged.promise
    cancellation.abort(new DOMException('receiver left during stage', 'AbortError'))
    await expect(load).rejects.toMatchObject({ name: 'AbortError' })

    stageGate.resolve()
    await store.aborted.promise
    expect(store.stages).toBe(1)
    expect(store.commits).toBe(0)
    client.close()
  })

  it('keeps a committed directory published when its last waiter cancels at the commit boundary', async () => {
    const fixture = catalogVectorFixture()
    const cancellation = new AbortController()
    let observerLoad: Promise<CommittedDirectory> | undefined
    const store = new ObservedCatalogStore(undefined, undefined, () => {
      observerLoad = observer.loadDirectory(fixture.directoryId)
      cancellation.abort(new DOMException('receiver left after publication', 'AbortError'))
    })
    const publisher = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: { fetchPage: async () => fixture.catalogPayload },
      store,
    })
    const observer = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: { fetchPage: async () => { throw new Error('committed directory was refetched') } },
      store,
    })

    const publishing = publisher.loadDirectory(fixture.directoryId, { signal: cancellation.signal })
    await expect(publishing).rejects.toMatchObject({ name: 'AbortError' })
    const pendingObserver = observerLoad
    if (pendingObserver === undefined) throw new Error('commit boundary did not start the observer')
    const committed = await pendingObserver
    // Drain the publisher's exclusive load lock before closing the store so any
    // late post-commit abort would have run while the committed page is verified.
    await withCatalogLoadLock(
      JSON.stringify(['windshare:v2:catalog', fixture.descriptor.shareInstanceId, committed.directoryIdText]),
      new AbortController().signal,
      async () => undefined,
    )

    expect(store.commits).toBe(1)
    expect(store.aborts).toBe(0)
    await expect(observer.page(committed, 0)).resolves.toBeDefined()
    await expect(store.loadPage(committed, 0)).resolves.toBeDefined()

    publisher.close()
    await store.closed.promise
    expect(store.aborts).toBe(0)
    observer.close()
  })

  it('keeps a local page-store failure out of authenticated protocol failure handling', async () => {
    const fixture = catalogVectorFixture()
    const localFailure = new CatalogPageStoreError(
      'local-storage',
      'IndexedDB quota unavailable',
    )
    const store = new ObservedCatalogStore(undefined, localFailure)
    const protocolFailures: unknown[] = []
    const client = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: {
        fetchPage: async () => fixture.catalogPayload,
        failProtocol: async (reason) => { protocolFailures.push(reason) },
      },
      store,
    })
    await expect(client.loadDirectory(fixture.directoryId)).rejects.toBe(localFailure)
    expect(protocolFailures).toEqual([])
    expect(store.aborts).toBe(1)
    client.close()
  })

  it('serializes the first load and reuses the committed directory', async () => {
    const identity = loadVectorFile(
      new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
    ).cases[0] as IdentityVector
    const catalogPayload = contentPayload('catalog-page')
    const directoryId = Uint8Array.from({ length: 16 }, (_, index) => 0x50 + index)
    const descriptor: ShareDescriptor = Object.freeze({
      wireVersion: 2,
      suite: 2,
      shareInstance: bytes(identity.shareInstanceB64),
      shareInstanceId: 'vector-share',
      syntheticRoot: directoryId,
      syntheticRootId: 'vector-root',
      chunkSize: 1 << 20,
      capabilities: 0n,
      sender: createEd25519Verifier(bytes(identity.senderPublicKeyB64)),
      createdAtSeconds: 1n,
      pathPolicy: PATH_POLICY,
    })
    const store = new MemoryCatalogPageStore()
    let fetches = 0
    const operations = {
      fetchPage: async () => {
        fetches += 1
        await Promise.resolve()
        return catalogPayload
      },
    }
    const options = {
      descriptor,

      operations,
      store,
      storageIdentity: 'capability-authority',
    }
    const first = new CatalogClient(options)
    const second = new CatalogClient(options)

    const [left, right] = await Promise.all([
      first.loadDirectory(directoryId),
      second.loadDirectory(directoryId),
    ])

    expect(fetches).toBe(1)
    expect(right).toEqual(left)
    first.close()
    second.close()
  })

  it('keeps the process-local lock published when a queued receiver cancels', async () => {
    let releaseFirst!: () => void
    const firstGate = new Promise<void>((resolve) => {
      releaseFirst = resolve
    })
    const entries: string[] = []
    const first = withCatalogLoadLock(
      'cancel-safe-lock',
      new AbortController().signal,
      async () => {
        entries.push('first')
        await firstGate
        return 'first'
      },
    )
    await Promise.resolve()
    const cancelledController = new AbortController()
    const cancelled = withCatalogLoadLock(
      'cancel-safe-lock',
      cancelledController.signal,
      async () => {
        entries.push('cancelled')
        return 'cancelled'
      },
    )
    const cancelledRejection = expect(cancelled).rejects.toMatchObject({ name: 'AbortError' })
    const third = withCatalogLoadLock(
      'cancel-safe-lock',
      new AbortController().signal,
      async () => {
        entries.push('third')
        return 'third'
      },
    )
    cancelledController.abort(new DOMException('receiver left', 'AbortError'))
    await cancelledRejection
    await Promise.resolve()

    expect(entries).toEqual(['first'])
    releaseFirst()
    await expect(first).resolves.toBe('first')
    await expect(third).resolves.toBe('third')
    expect(entries).toEqual(['first', 'third'])
  })

  it('isolates hostile scan-progress listeners from the catalog operation and each other', async () => {
    const identity = loadVectorFile(
      new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
    ).cases[0] as IdentityVector
    const catalogPayload = contentPayload('catalog-page')
    const directoryId = Uint8Array.from({ length: 16 }, (_, index) => 0x50 + index)
    const descriptor: ShareDescriptor = Object.freeze({
      wireVersion: 2,
      suite: 2,
      shareInstance: bytes(identity.shareInstanceB64),
      shareInstanceId: 'vector-share',
      syntheticRoot: directoryId,
      syntheticRootId: 'vector-root',
      chunkSize: 1 << 20,
      capabilities: 0n,
      sender: createEd25519Verifier(bytes(identity.senderPublicKeyB64)),
      createdAtSeconds: 1n,
      pathPolicy: PATH_POLICY,
    })
    const client = new CatalogClient({
      descriptor,
      operations: {
        fetchPage: async (_request, _signal, onProgress) => {
          onProgress?.({
            directoryId,
            attemptId: Uint8Array.from({ length: 16 }, () => 0x31),
            discoveredEntries: 257n,
          })
          return catalogPayload
        },
      },
      store: new MemoryCatalogPageStore(),
    })
    client.subscribeScanProgress((progress) => {
      progress.directoryId[0] = 0
      throw new Error('hostile listener')
    })
    const observed: number[] = []
    client.subscribeScanProgress((progress) => observed.push(progress.directoryId[0] ?? 0))

    await expect(client.loadDirectory(directoryId)).resolves.toBeDefined()
    expect(observed).toEqual([0x50])
    client.close()
  })

  it('skips navigator.locks when backed by a process-local store even if Web Locks denies access', async () => {
    const fixture = catalogVectorFixture()
    const request = vi.fn(async () => {
      throw new DOMException('Access to the Locks API is denied in this context', 'SecurityError')
    })
    const originalLocks = Object.getOwnPropertyDescriptor(globalThis.navigator, 'locks')
    Object.defineProperty(globalThis.navigator, 'locks', {
      configurable: true,
      value: { request },
    })
    const client = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: { fetchPage: async () => fixture.catalogPayload },
      store: new MemoryCatalogPageStore(),
    })
    try {
      const committed = await client.loadDirectory(fixture.directoryId)
      expect(committed.pageCount).toBe(1)
      expect(request).not.toHaveBeenCalled()
    } finally {
      client.close()
      if (originalLocks === undefined) {
        delete (globalThis.navigator as { locks?: unknown }).locks
      } else {
        Object.defineProperty(globalThis.navigator, 'locks', originalLocks)
      }
    }
  })

  it('rejects page and entry iteration with CatalogClientError once closed', async () => {
    const fixture = catalogVectorFixture()
    const client = new CatalogClient({
      descriptor: fixture.descriptor,
      operations: { fetchPage: async () => fixture.catalogPayload },
      store: new MemoryCatalogPageStore(),
    })
    const committed = await client.loadDirectory(fixture.directoryId)
    const multiPageHandle = Object.freeze({ ...committed, pageCount: 2 })

    const iterator = client.pages(multiPageHandle)
    const first = await iterator.next()
    expect(first.done).toBe(false)

    client.close()
    await expect(iterator.next()).rejects.toThrow('Catalog client is closed')
    await expect(client.pages(committed).next()).rejects.toBeInstanceOf(CatalogClientError)
    await expect(client.entries(committed).next()).rejects.toBeInstanceOf(CatalogClientError)
    await expect(client.page(committed, 0)).rejects.toBeInstanceOf(CatalogClientError)
  })
})

function catalogVectorFixture(): {
  readonly descriptor: ShareDescriptor

  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly catalogPayload: Uint8Array<ArrayBuffer>
} {
  const identity = loadVectorFile(
    new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
  ).cases[0] as IdentityVector
  const catalogPayload = contentPayload('catalog-page')
  const directoryId = Uint8Array.from({ length: 16 }, (_, index) => 0x50 + index)
  return Object.freeze({
    descriptor: Object.freeze({
      wireVersion: 2,
      suite: 2,
      shareInstance: bytes(identity.shareInstanceB64),
      shareInstanceId: 'vector-share',
      syntheticRoot: directoryId,
      syntheticRootId: 'vector-root',
      chunkSize: 1 << 20,
      capabilities: 0n,
      sender: createEd25519Verifier(bytes(identity.senderPublicKeyB64)),
      createdAtSeconds: 1n,
      pathPolicy: PATH_POLICY,
    }),

    directoryId,
    catalogPayload,
  })
}

class ObservedCatalogStore implements CatalogPageStore {
  readonly #inner = new MemoryCatalogPageStore()
  readonly #stageGate: Promise<void> | undefined
  readonly #stageFailure: unknown
  readonly #afterCommit: (() => void) | undefined
  readonly staged = deferred<void>()
  readonly aborted = deferred<void>()
  readonly closed = deferred<void>()
  stages = 0
  commits = 0
  aborts = 0
  closes = 0

  constructor(stageGate?: Promise<void>, stageFailure?: unknown, afterCommit?: () => void) {
    this.#stageGate = stageGate
    this.#stageFailure = stageFailure
    this.#afterCommit = afterCommit
  }

  loadDirectory(directoryIdText: string): Promise<CommittedDirectory | undefined> {
    return this.#inner.loadDirectory(directoryIdText)
  }

  loadFailure(directoryIdText: string) {
    return this.#inner.loadFailure(directoryIdText)
  }

  loadPage(directory: CommittedDirectory, pageIndex: number): Promise<CatalogPage | undefined> {
    return this.#inner.loadPage(directory, pageIndex)
  }

  begin(directoryIdText: string): Promise<void> {
    return this.#inner.begin(directoryIdText)
  }

  async stage(page: CatalogPage): Promise<void> {
    this.stages += 1
    if (this.#stageFailure !== undefined) throw this.#stageFailure
    await this.#inner.stage(page)
    this.staged.resolve()
    await this.#stageGate
  }

  async commit(directory: CommittedDirectory): Promise<void> {
    this.commits += 1
    await this.#inner.commit(directory)
    this.#afterCommit?.()
  }

  storeFailure(cached: Parameters<CatalogPageStore['storeFailure']>[0]): Promise<void> {
    return this.#inner.storeFailure(cached)
  }

  async abort(directoryIdText: string): Promise<void> {
    this.aborts += 1
    await this.#inner.abort(directoryIdText)
    this.aborted.resolve()
  }

  close(): void {
    this.closes += 1
    this.#inner.close()
    this.closed.resolve()
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  resolve(value: T): void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((complete) => { resolve = complete })
  return { promise, resolve }
}
