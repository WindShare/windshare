import { describe, expect, it } from 'vitest'

import {
  catalogPageMetadataCharge,
  MemoryV2CatalogPageStore,
  V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS,
} from '../../src/catalog/v2-page-store'
import {
  type V2CatalogEntry,
  type V2CatalogPage,
  type V2DirectoryFailure,
  V2_CATALOG_DIRECTORY_ENTRIES,
  V2_CATALOG_PAGE_ENTRIES,
  V2_CATALOG_PAGE_OBJECT_BYTES,
} from '../../src/catalog/v2-records'

function entry(id: number, name: string): V2CatalogEntry {
  const identity = new Uint8Array(16)
  identity[0] = id
  return { kind: 'file', id: identity, idText: `node-${id}`, name, expectedSize: 1n }
}

function page(index: number, directory: string, entries: readonly V2CatalogEntry[]): V2CatalogPage {
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
    senderObjectBytes: 1_024,
  }
}

describe('v2 catalog page store', () => {
  it('binds browser-profile budgets to the frozen cross-runtime limits', () => {
    expect(V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS).toEqual({
      entriesPerShare: 4_194_304,
      entriesPerProfile: 16_777_216,
      metadataBytesPerShare: 2_147_483_648,
      metadataBytesPerProfile: 17_179_869_184,
    })
    expect(catalogPageMetadataCharge(page(0, 'root', [entry(1, 'first')]))).toBe(20_992)
    const maximumPageCharge = catalogPageMetadataCharge({
      ...page(0, 'root', Array.from(
        { length: V2_CATALOG_PAGE_ENTRIES },
        () => entry(1, 'maximum'),
      )),
      senderObjectBytes: V2_CATALOG_PAGE_OBJECT_BYTES,
    })
    expect(maximumPageCharge).toBe(393_216)
    expect(
      maximumPageCharge * Math.ceil(V2_CATALOG_DIRECTORY_ENTRIES / V2_CATALOG_PAGE_ENTRIES),
    ).toBeLessThanOrEqual(V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS.metadataBytesPerShare)
    expect(() => catalogPageMetadataCharge({
      ...page(0, 'root', []),
      senderObjectBytes: 0,
    })).toThrow(/durable footprint/iu)
  })

  it('rejects full Unicode case-fold collisions across page boundaries', async () => {
    const store = new MemoryV2CatalogPageStore()
    await store.begin('root')
    await store.stage(page(0, 'root', [entry(1, 'STRASSE')]))

    await expect(store.stage(page(1, 'root', [entry(2, 'Straße')])))
      .rejects.toMatchObject({
        name: 'V2CatalogPageStoreError',
        kind: 'authenticated-ownership',
      })
  })

  it('scopes portable names to their owning directory', async () => {
    const store = new MemoryV2CatalogPageStore()
    await store.stage(page(0, 'first', [entry(1, 'Straße')]))
    await expect(store.stage(page(0, 'second', [entry(2, 'STRASSE')])))
      .resolves.toBeUndefined()
  })

  it('atomically replaces staged ownership with persistent failure authority', async () => {
    const store = new MemoryV2CatalogPageStore()
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
    const store = new MemoryV2CatalogPageStore()
    const first = { ...page(0, 'a\0b', [entry(1, 'first')]), generationText: 'c' }
    const second = { ...page(0, 'a', [entry(2, 'second')]), generationText: 'b\0c' }
    await store.stage(first)
    await store.stage(second)
    await store.abort('a')

    await expect(store.loadPage({
      directoryIdText: 'a\0b',
      generationText: 'c',
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    }, 0)).resolves.toMatchObject({ directoryIdText: 'a\0b', generationText: 'c' })
  })

  it('classifies invalid local cached state separately from authenticated ownership', async () => {
    const store = new MemoryV2CatalogPageStore()
    await expect(store.storeFailure({
      failure: directoryFailure('root', true),
      retryAtMilliseconds: null,
    })).rejects.toMatchObject({
      name: 'V2CatalogPageStoreError',
      kind: 'local-storage',
    })
  })
})

function directoryFailure(directoryIdText: string, retryable: boolean): V2DirectoryFailure {
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
