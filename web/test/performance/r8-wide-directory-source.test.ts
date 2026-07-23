import { describe, expect, it } from 'vitest'

import { MemoryV2CatalogPageStore } from '../../src/catalog/v2-page-store'
import { V2_CATALOG_PAGE_ENTRIES } from '../../src/catalog/v2-records'
import {
  catalogEntryName,
  createR8WideDirectoryFixture,
  ObservedR8CatalogPageStore,
  type R8WideDirectoryProgress,
} from './r8-wide-directory-source'

describe('R8 wide-directory production path', () => {
  it('generates, authenticates, stages, and reloads pages without an eager entry array', async () => {
    const entryCount = V2_CATALOG_PAGE_ENTRIES * 2
    const progress: R8WideDirectoryProgress[] = []
    const fixture = await createR8WideDirectoryFixture(entryCount, (update) => progress.push(update))
    const store = new ObservedR8CatalogPageStore(
      new MemoryV2CatalogPageStore(),
      fixture.probe,
    )
    const client = fixture.createClient(store, 'r8-wide-unit')
    try {
      expect(fixture.probe.snapshot()).toMatchObject({
        generatedPages: 0,
        generatedEntries: 0,
        stagedPages: 0,
        stagedEntries: 0,
        stagedPageOwnershipRecords: 0,
        stagedNodeOwnershipKeys: 0,
        stagedNameOwnershipKeys: 0,
      })

      const committed = await client.loadDirectory(fixture.descriptor.syntheticRoot)
      expect(committed).toMatchObject({ pageCount: 2, entryCount })
      expect(fixture.probe.snapshot()).toMatchObject({
        generatedPages: 2,
        generatedEntries: entryCount,
        stagedPages: 2,
        stagedEntries: entryCount,
        stagedPageOwnershipRecords: 2,
        stagedNodeOwnershipKeys: entryCount,
        stagedNameOwnershipKeys: entryCount,
        maximumGeneratedRows: V2_CATALOG_PAGE_ENTRIES,
        maximumSourceSenderObjects: 1,
        maximumStoreBoundaryPages: 1,
        maximumStoreBoundaryRows: V2_CATALOG_PAGE_ENTRIES,
        protocolFailures: 0,
      })
      expect(progress).toEqual([{
        pageIndex: 1,
        stagedPages: 2,
        stagedEntries: entryCount,
        stagedPageOwnershipRecords: 2,
        stagedNodeOwnershipKeys: entryCount,
        stagedNameOwnershipKeys: entryCount,
      }])

      const first = await client.page(committed, 0)
      const second = await client.page(committed, 1)
      expect(first.entries).toHaveLength(V2_CATALOG_PAGE_ENTRIES)
      expect(second.entries).toHaveLength(V2_CATALOG_PAGE_ENTRIES)
      expect(first.entries[0]?.name).toBe(catalogEntryName(0))
      expect(first.entries.at(-1)?.name).toBe(catalogEntryName(V2_CATALOG_PAGE_ENTRIES - 1))
      expect(second.entries[0]?.name).toBe(catalogEntryName(V2_CATALOG_PAGE_ENTRIES))
      expect(second.entries.at(-1)?.name).toBe(catalogEntryName(entryCount - 1))
      expect(fixture.probe.snapshot()).toMatchObject({
        loadedPages: 2,
        maximumLoadedPageRows: V2_CATALOG_PAGE_ENTRIES,
      })
    } finally {
      client.close()
      fixture.close()
    }
  })

  it('refuses a page request that bypasses the production sequential client', async () => {
    const fixture = await createR8WideDirectoryFixture(V2_CATALOG_PAGE_ENTRIES)
    try {
      await expect(fixture.operations.fetchPage({
        directoryId: fixture.descriptor.syntheticRoot,
        pageIndex: 1,
        generation: new Uint8Array(16).fill(0x33),
      }, new AbortController().signal)).rejects.toThrow(/out-of-order/u)
      expect(fixture.probe.snapshot()).toMatchObject({
        generatedPages: 0,
        generatedEntries: 0,
        stagedPages: 0,
      })
    } finally {
      fixture.close()
    }
  })
})
