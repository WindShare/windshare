import { describe, expect, it } from 'vitest'

import {
  catalogPageMetadataCharge,
  CatalogPageStoreError,
  DEFAULT_MEMORY_CATALOG_CACHE_BUDGET_LIMITS,
  MemoryCatalogPageStore,
} from '../../src/catalog/page-store'
import {
  type CatalogEntry,
  type CatalogPage,
  type DirectoryFailure,
  CATALOG_PAGE_ENTRIES,
  MAX_CATALOG_PAYLOAD_BYTES,
} from '../../src/catalog/records'

function entry(id: number, name: string): CatalogEntry {
  const identity = new Uint8Array(16)
  identity[0] = id
  return { kind: 'file', id: identity, idText: `node-${id}`, name, expectedSize: 1n }
}

function page(index: number, directory: string, entries: readonly CatalogEntry[]): CatalogPage {
  const directoryId = new Uint8Array(16)
  directoryId[0] = directory.charCodeAt(0)
  const generation = new Uint8Array(16)
  generation[0] = 1
  return {
    shareInstance: new Uint8Array(16),
    directoryId,
    directoryIdText: directory,
    generation,
    generationText: 'generation',
    pageIndex: index,
    terminal: index === 1,
    previousCommitment: new Uint8Array(32),
    entries,
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    encodedBytes: 1_024,
  }
}

describe('v2 catalog page store', () => {
  it('computes bounded in-memory page metadata charges and rejects invalid footprints', () => {
    expect(DEFAULT_MEMORY_CATALOG_CACHE_BUDGET_LIMITS).toEqual({
      maxEntries: 524_288,
      maxMetadataBytes: 256 * 1024 * 1024,
    })
    expect(catalogPageMetadataCharge(page(0, 'root', [entry(1, 'first')]))).toBe(6_656)
    const maximumPageCharge = catalogPageMetadataCharge({
      ...page(0, 'root', Array.from(
        { length: CATALOG_PAGE_ENTRIES },
        () => entry(1, 'maximum'),
      )),
      encodedBytes: MAX_CATALOG_PAYLOAD_BYTES,
    })
    expect(maximumPageCharge).toBe(378_880)
    expect(() => catalogPageMetadataCharge({
      ...page(0, 'root', []),
      encodedBytes: 0,
    })).toThrow(/memory footprint/iu)
  })

  it('rejects full Unicode case-fold collisions across page boundaries', async () => {
    const store = new MemoryCatalogPageStore()
    await store.begin('root')
    await store.stage(page(0, 'root', [entry(1, 'STRASSE')]))

    await expect(store.stage(page(1, 'root', [entry(2, 'Straße')])))
      .rejects.toMatchObject({
        name: 'CatalogPageStoreError',
        kind: 'authenticated-ownership',
      })
  })

  it('scopes portable names to their owning directory', async () => {
    const store = new MemoryCatalogPageStore()
    await store.stage(page(0, 'first', [entry(1, 'Straße')]))
    await expect(store.stage(page(0, 'second', [entry(2, 'STRASSE')])))
      .resolves.toBeUndefined()
  })

  it('atomically replaces staged ownership with cached failure authority', async () => {
    const store = new MemoryCatalogPageStore()
    await store.stage(page(0, 'root', [entry(1, 'first')]))
    await store.storeFailure({
      failure: directoryFailure('root', true),
      retryAtMilliseconds: 1_000,
    })

    await expect(store.loadFailure('root')).resolves.toMatchObject({
      failure: { directoryIdText: 'root', retryable: true },
      retryAtMilliseconds: 1_000,
    })
    await expect(store.loadPage({
      directoryIdText: 'root',
      generationText: 'generation',
      directoryId: new Uint8Array(16).fill(1),
      generation: new Uint8Array(16).fill(2),
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    }, 0)).resolves.toBeUndefined()
    await expect(store.stage(page(0, 'other', [entry(1, 'first')]))).resolves.toBeUndefined()

    await store.begin('root')
    await expect(store.loadFailure('root')).resolves.toBeUndefined()
  })

  it('keeps delimiter-bearing directory and generation scopes structurally distinct', async () => {
    const store = new MemoryCatalogPageStore()
    const first = { ...page(0, 'a\0b', [entry(1, 'first')]), generationText: 'c' }
    const second = { ...page(0, 'a', [entry(2, 'second')]), generationText: 'b\0c' }
    await store.stage(first)
    await store.stage(second)
    await store.abort('a')

    await expect(store.loadPage({
      directoryIdText: 'a\0b',
      generationText: 'c',
      directoryId: new Uint8Array(16).fill(1),
      generation: new Uint8Array(16).fill(2),
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    }, 0)).resolves.toMatchObject({ directoryIdText: 'a\0b', generationText: 'c' })
  })

  it('classifies invalid local cached state separately from authenticated ownership', async () => {
    const store = new MemoryCatalogPageStore()
    await expect(store.storeFailure({
      failure: directoryFailure('root', true),
      retryAtMilliseconds: null,
    })).rejects.toMatchObject({
      name: 'CatalogPageStoreError',
      kind: 'local-storage',
    })
  })

  it('enforces bounded in-memory budgets and reclaims charges on abort and failure', async () => {
    const first = page(0, 'first', [entry(1, 'alpha')])
    const singlePageBytes = catalogPageMetadataCharge(first)
    const store = new MemoryCatalogPageStore({
      maxEntries: 2,
      maxMetadataBytes: singlePageBytes * 2,
    })

    await store.stage(first)
    await store.stage(page(0, 'second', [entry(2, 'beta')]))

    // Exceeding the entry/byte budget rejects atomically without claiming node or name ownership.
    await expect(store.stage(page(0, 'third', [entry(3, 'gamma')])))
      .rejects.toMatchObject({
        name: 'CatalogPageStoreError',
        kind: 'resource-limit',
      })

    await store.abort('second')
    await expect(store.stage(page(0, 'third', [entry(3, 'gamma')]))).resolves.toBeUndefined()

    await store.storeFailure({
      failure: directoryFailure('third', false),
      retryAtMilliseconds: null,
    })
    await expect(store.stage(page(0, 'fourth', [entry(4, 'delta')]))).resolves.toBeUndefined()

    expect(() => new MemoryCatalogPageStore({ maxEntries: 0, maxMetadataBytes: singlePageBytes }))
      .toThrow(CatalogPageStoreError)
  })

  it('releases node ownership across all staged pages and generations of an aborted directory only', async () => {
    const store = new MemoryCatalogPageStore()
    await store.stage(page(0, 'alpha', [entry(1, 'one'), entry(2, 'two')]))
    await store.stage(page(1, 'alpha', [entry(3, 'three')]))
    await store.stage({ ...page(0, 'alpha', [entry(4, 'four')]), generationText: 'generation-2' })
    await store.stage(page(0, 'beta', [entry(5, 'five')]))

    await store.abort('alpha')

    await expect(store.stage(page(0, 'gamma', [
      entry(1, 'one'),
      entry(2, 'two'),
      entry(3, 'three'),
      entry(4, 'four'),
    ]))).resolves.toBeUndefined()

    await expect(store.stage(page(1, 'gamma', [entry(5, 'five')])))
      .rejects.toMatchObject({
        name: 'CatalogPageStoreError',
        kind: 'authenticated-ownership',
      })
  })

  it('avoids share-wide map scans when beginning new directories or aborting an existing one', async () => {
    const store = new MemoryCatalogPageStore()
    const directoryCount = 200
    const entriesPerDirectory = 4
    let nextNodeId = 1
    for (let dirIndex = 0; dirIndex < directoryCount; dirIndex += 1) {
      const directoryId = `dir-${dirIndex}`
      const entries = Array.from({ length: entriesPerDirectory }, (_, itemIndex) =>
        entry(nextNodeId++, `file-${itemIndex}`),
      )
      await store.begin(directoryId)
      await store.stage(page(0, directoryId, entries))
    }

    const beginFreshVisits = await countMapIterations(() => store.begin('brand-new-dir'))
    expect(beginFreshVisits).toBe(0)

    const abortExistingVisits = await countMapIterations(() => store.abort('dir-0'))
    expect(abortExistingVisits).toBeLessThanOrEqual(4)
  })

  it('supports wide directories and cumulative multi-directory browsing within default share budgets', async () => {
    const wideStore = new MemoryCatalogPageStore()
    const widePages = 100
    let nextWideId = 1
    await wideStore.begin('wide-dir')
    for (let pageIndex = 0; pageIndex < widePages; pageIndex += 1) {
      const entries = Array.from({ length: CATALOG_PAGE_ENTRIES }, (_, slot) =>
        entry(nextWideId++, `file-${pageIndex}-${slot}`),
      )
      await wideStore.stage({
        ...page(pageIndex, 'wide-dir', entries),
        terminal: pageIndex === widePages - 1,
        encodedBytes: 16_384,
      })
    }
    await wideStore.commit({
      directoryIdText: 'wide-dir',
      generationText: 'generation',
      directoryId: new Uint8Array(16).fill(1),
      generation: new Uint8Array(16).fill(2),
      pageCount: widePages,
      entryCount: widePages * CATALOG_PAGE_ENTRIES,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    })
    const loadedWide = await wideStore.loadDirectory('wide-dir')
    expect(loadedWide?.entryCount).toBe(25_600)

    // With the calibrated 2 KiB in-memory per-page base, 20,000 small directories
    // (5 files each, 100,000 entries total) fit cleanly within the 256 MiB share
    // budget instead of tripping an artificial 16 KiB/page IndexedDB ceiling.
    const nestedStore = new MemoryCatalogPageStore()
    const smallDirectories = 20_000
    const entriesPerSmallDir = 5
    const encodedSmallPageBytes = 400
    let nextNestedId = 1
    for (let dirIndex = 0; dirIndex < smallDirectories; dirIndex += 1) {
      const dirId = `small-${dirIndex}`
      const entries = Array.from({ length: entriesPerSmallDir }, (_, slot) =>
        entry(nextNestedId++, `item-${slot}`),
      )
      await nestedStore.stage({
        ...page(0, dirId, entries),
        terminal: true,
        encodedBytes: encodedSmallPageBytes,
      })
    }
    expect(await nestedStore.loadPage({
      directoryIdText: 'small-19999',
      generationText: 'generation',
      directoryId: new Uint8Array(16).fill(1),
      generation: new Uint8Array(16).fill(2),
      pageCount: 1,
      entryCount: entriesPerSmallDir,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    }, 0)).toBeDefined()
  })
})

async function countMapIterations(operation: () => Promise<void>): Promise<number> {
  let visited = 0
  const originalIterator = Map.prototype[Symbol.iterator]
  const originalEntries = Map.prototype.entries
  const originalValues = Map.prototype.values
  const originalKeys = Map.prototype.keys
  const originalForEach = Map.prototype.forEach

  function wrap<T>(
    original: (this: Map<unknown, unknown>) => MapIterator<T>,
  ): (this: Map<unknown, unknown>) => MapIterator<T> {
    return function (this: Map<unknown, unknown>): MapIterator<T> {
      const inner = original.call(this)
      return (function* (): Generator<T, undefined, undefined> {
        for (const item of inner) {
          visited += 1
          yield item
        }
        return undefined
      })()
    }
  }

  Map.prototype[Symbol.iterator] = wrap(originalIterator)
  Map.prototype.entries = wrap(originalEntries)
  Map.prototype.values = wrap(originalValues)
  Map.prototype.keys = wrap(originalKeys)
  Map.prototype.forEach = function (callback, thisArg) {
    return originalForEach.call(this, (value, key, map) => {
      visited += 1
      callback.call(thisArg, value, key, map)
    })
  }
  try {
    await operation()
  } finally {
    Map.prototype[Symbol.iterator] = originalIterator
    Map.prototype.entries = originalEntries
    Map.prototype.values = originalValues
    Map.prototype.keys = originalKeys
    Map.prototype.forEach = originalForEach
  }
  return visited
}

function directoryFailure(directoryIdText: string, retryable: boolean): DirectoryFailure {
  return Object.freeze({
    shareInstance: new Uint8Array(16).fill(1),
    directoryId: new Uint8Array(16).fill(2),
    directoryIdText,
    attemptId: new Uint8Array(16).fill(3),
    attemptIdText: 'attempt',
    code: retryable ? 0x2007 : 0x2006,
    kind: retryable ? 'retryable' : 'permanent',
    retryable,
    ...(retryable ? { retryAfterMilliseconds: 250 } : {}),
  })
}
