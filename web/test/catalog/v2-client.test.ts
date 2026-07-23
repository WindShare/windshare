import { describe, expect, it } from 'vitest'

import {
  V2CatalogClient,
  V2CatalogClientError,
  withV2CatalogLoadLock,
} from '../../src/catalog/v2-client'
import {
  MemoryV2CatalogPageStore,
  V2CatalogPageStoreError,
  type V2CatalogPageStore,
  type V2CommittedDirectory,
} from '../../src/catalog/v2-page-store'
import {
  V2_PATH_POLICY,
  type V2CatalogPage,
  type V2ShareDescriptor,
} from '../../src/catalog/v2-records'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'
import { createSignedRootCollisionFixture } from './signed-root-collision-fixture'
import { runSignedCatalogOwnershipProbe } from './signed-catalog-ownership-probe'

interface IdentityVector extends VectorCase {
  readonly readSecretB64: string
  readonly senderPublicKeyB64: string
  readonly shareInstanceB64: string
}

interface SenderObjectVector extends VectorCase {
  readonly domain: string
  readonly objectB64: string
}

function bytes(encoded: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(b64ToBytes(encoded))
}

describe('v2 catalog multi-receiver ownership', () => {
  it('reports signed cross-page and cross-directory ownership collisions as protocol failures', async () => {
    await expect(runSignedCatalogOwnershipProbe(new MemoryV2CatalogPageStore())).resolves.toEqual({
      crossPageNameRejected: true,
      crossPageNodeRejected: true,
      crossDirectoryNodeRejected: true,
      everyCollisionReportedAsProtocol: true,
      firstDirectoryCommitSurvived: true,
    })
  })

  it('rejects an authenticated page that reintroduces the synthetic root as a child', async () => {
    const fixture = await createSignedRootCollisionFixture()
    const store = new MemoryV2CatalogPageStore()
    const protocolFailures: unknown[] = []
    const client = new V2CatalogClient({
      descriptor: fixture.descriptor,
      readSecret: fixture.readSecret,
      operations: {
        fetchPage: async () => fixture.catalogObject,
        failProtocol: async (reason) => { protocolFailures.push(reason) },
      },
      store,
    })
    try {
      await expect(client.loadDirectory(fixture.descriptor.syntheticRoot))
        .rejects.toBeInstanceOf(V2CatalogClientError)
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
    const client = new V2CatalogClient({
      descriptor: fixture.descriptor,
      readSecret: fixture.readSecret,
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
    const client = new V2CatalogClient({
      descriptor: fixture.descriptor,
      readSecret: fixture.readSecret,
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
    const client = new V2CatalogClient({
      descriptor: fixture.descriptor,
      readSecret: fixture.readSecret,
      operations: { fetchPage: async () => fixture.catalogObject },
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
    let observerLoad: Promise<V2CommittedDirectory> | undefined
    const store = new ObservedCatalogStore(undefined, undefined, () => {
      observerLoad = observer.loadDirectory(fixture.directoryId)
      cancellation.abort(new DOMException('receiver left after publication', 'AbortError'))
    })
    const publisher = new V2CatalogClient({
      descriptor: fixture.descriptor,
      readSecret: fixture.readSecret,
      operations: { fetchPage: async () => fixture.catalogObject },
      store,
    })
    const observer = new V2CatalogClient({
      descriptor: fixture.descriptor,
      readSecret: fixture.readSecret,
      operations: { fetchPage: async () => { throw new Error('committed directory was refetched') } },
      store,
    })

    const publishing = publisher.loadDirectory(fixture.directoryId, { signal: cancellation.signal })
    await expect(publishing).rejects.toMatchObject({ name: 'AbortError' })
    const pendingObserver = observerLoad
    if (pendingObserver === undefined) throw new Error('commit boundary did not start the observer')
    const committed = await pendingObserver
    publisher.close()
    await store.closed.promise

    expect(store.commits).toBe(1)
    expect(store.aborts).toBe(0)
    await expect(store.loadPage(committed, 0)).resolves.toBeDefined()
    observer.close()
  })

  it('keeps a local page-store failure out of authenticated protocol failure handling', async () => {
    const fixture = catalogVectorFixture()
    const localFailure = new V2CatalogPageStoreError(
      'local-storage',
      'IndexedDB quota unavailable',
    )
    const store = new ObservedCatalogStore(undefined, localFailure)
    const protocolFailures: unknown[] = []
    const client = new V2CatalogClient({
      descriptor: fixture.descriptor,
      readSecret: fixture.readSecret,
      operations: {
        fetchPage: async () => fixture.catalogObject,
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
    const catalogObject = loadVectorFile(
      new URL('../../../core/testvectors/v2-sender-objects.json', import.meta.url),
    ).cases.find((candidate) => (candidate as SenderObjectVector).domain ===
      'windshare/v2 object/catalog-page') as SenderObjectVector
    const directoryId = Uint8Array.from({ length: 16 }, (_, index) => 0x50 + index)
    const descriptor: V2ShareDescriptor = Object.freeze({
      wireVersion: 2,
      suite: 2,
      shareInstance: bytes(identity.shareInstanceB64),
      shareInstanceId: 'vector-share',
      syntheticRoot: directoryId,
      syntheticRootId: 'vector-root',
      chunkSize: 1 << 20,
      capabilities: 0n,
      senderPublicKey: bytes(identity.senderPublicKeyB64),
      createdAtSeconds: 1n,
      pathPolicy: V2_PATH_POLICY,
    })
    const store = new MemoryV2CatalogPageStore()
    let fetches = 0
    const operations = {
      fetchPage: async () => {
        fetches += 1
        await Promise.resolve()
        return bytes(catalogObject.objectB64)
      },
    }
    const options = {
      descriptor,
      readSecret: bytes(identity.readSecretB64),
      operations,
      store,
      storageIdentity: 'capability-authority',
    }
    const first = new V2CatalogClient(options)
    const second = new V2CatalogClient(options)

    const [left, right] = await Promise.all([
      first.loadDirectory(directoryId),
      second.loadDirectory(directoryId),
    ])

    expect(fetches).toBe(1)
    expect(right).toEqual(left)
    first.close()
    second.close()
  })

  it('keeps the fallback lock published when a queued receiver cancels', async () => {
    let releaseFirst!: () => void
    const firstGate = new Promise<void>((resolve) => {
      releaseFirst = resolve
    })
    const entries: string[] = []
    const first = withV2CatalogLoadLock(
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
    const cancelled = withV2CatalogLoadLock(
      'cancel-safe-lock',
      cancelledController.signal,
      async () => {
        entries.push('cancelled')
        return 'cancelled'
      },
    )
    const cancelledRejection = expect(cancelled).rejects.toMatchObject({ name: 'AbortError' })
    const third = withV2CatalogLoadLock(
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
    const catalogObject = loadVectorFile(
      new URL('../../../core/testvectors/v2-sender-objects.json', import.meta.url),
    ).cases.find((candidate) => (candidate as SenderObjectVector).domain ===
      'windshare/v2 object/catalog-page') as SenderObjectVector
    const directoryId = Uint8Array.from({ length: 16 }, (_, index) => 0x50 + index)
    const descriptor: V2ShareDescriptor = Object.freeze({
      wireVersion: 2,
      suite: 2,
      shareInstance: bytes(identity.shareInstanceB64),
      shareInstanceId: 'vector-share',
      syntheticRoot: directoryId,
      syntheticRootId: 'vector-root',
      chunkSize: 1 << 20,
      capabilities: 0n,
      senderPublicKey: bytes(identity.senderPublicKeyB64),
      createdAtSeconds: 1n,
      pathPolicy: V2_PATH_POLICY,
    })
    const client = new V2CatalogClient({
      descriptor,
      readSecret: bytes(identity.readSecretB64),
      operations: {
        fetchPage: async (_request, _signal, onProgress) => {
          onProgress?.({
            directoryId,
            attemptId: Uint8Array.from({ length: 16 }, () => 0x31),
            discoveredEntries: 257n,
          })
          return bytes(catalogObject.objectB64)
        },
      },
      store: new MemoryV2CatalogPageStore(),
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
})

function catalogVectorFixture(): {
  readonly descriptor: V2ShareDescriptor
  readonly readSecret: Uint8Array<ArrayBuffer>
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly catalogObject: Uint8Array<ArrayBuffer>
} {
  const identity = loadVectorFile(
    new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
  ).cases[0] as IdentityVector
  const catalogObject = loadVectorFile(
    new URL('../../../core/testvectors/v2-sender-objects.json', import.meta.url),
  ).cases.find((candidate) => (candidate as SenderObjectVector).domain ===
    'windshare/v2 object/catalog-page') as SenderObjectVector
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
      senderPublicKey: bytes(identity.senderPublicKeyB64),
      createdAtSeconds: 1n,
      pathPolicy: V2_PATH_POLICY,
    }),
    readSecret: bytes(identity.readSecretB64),
    directoryId,
    catalogObject: bytes(catalogObject.objectB64),
  })
}

class ObservedCatalogStore implements V2CatalogPageStore {
  readonly #inner = new MemoryV2CatalogPageStore()
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

  loadDirectory(directoryIdText: string): Promise<V2CommittedDirectory | undefined> {
    return this.#inner.loadDirectory(directoryIdText)
  }

  loadFailure(directoryIdText: string) {
    return this.#inner.loadFailure(directoryIdText)
  }

  loadPage(directory: V2CommittedDirectory, pageIndex: number): Promise<V2CatalogPage | undefined> {
    return this.#inner.loadPage(directory, pageIndex)
  }

  begin(directoryIdText: string): Promise<void> {
    return this.#inner.begin(directoryIdText)
  }

  async stage(page: V2CatalogPage): Promise<void> {
    this.stages += 1
    if (this.#stageFailure !== undefined) throw this.#stageFailure
    await this.#inner.stage(page)
    this.staged.resolve()
    await this.#stageGate
  }

  async commit(directory: V2CommittedDirectory): Promise<void> {
    this.commits += 1
    await this.#inner.commit(directory)
    this.#afterCommit?.()
  }

  storeFailure(cached: Parameters<V2CatalogPageStore['storeFailure']>[0]): Promise<void> {
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
