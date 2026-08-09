import { describe, expect, it } from 'vitest'

import {
  IndexedDbZipCentralDirectorySpool,
  ZipSpoolBudgetExceededError,
  ZIP_SPOOL_MAXIMUM_BYTES,
  ZIP_SPOOL_MAXIMUM_ENTRIES,
} from '../../src/output/streams/zip-spool'

const ZIP_SPOOL_CHUNK_BYTES = 256 * 1024

describe('IndexedDB ZIP central-directory operation budgets', () => {
  it('rejects the next entry before accepting any part of it', async () => {
    const spool = new IndexedDbZipCentralDirectorySpool({
      namespace: 'entry-budget',
      token: 'entry-budget-token',
      maxEntries: 1,
      maxBytes: 8,
    })
    await spool.append(Uint8Array.of(0x11, 0x12))

    await expect(spool.append(Uint8Array.of(0x21))).rejects.toEqual(
      new ZipSpoolBudgetExceededError('zip-central-directory-entries', 1n, 2n),
    )
    await spool.clear()
  })

  it('does not charge a rejected byte reservation', async () => {
    const spool = new IndexedDbZipCentralDirectorySpool({
      namespace: 'byte-budget',
      token: 'byte-budget-token',
      maxEntries: 4,
      maxBytes: 3,
    })
    await spool.append(Uint8Array.of(0x31, 0x32))

    await expect(spool.append(Uint8Array.of(0x41, 0x42))).rejects.toEqual(
      new ZipSpoolBudgetExceededError('zip-central-directory-bytes', 3n, 4n),
    )
    await expect(spool.append(Uint8Array.of(0x51))).resolves.toBeUndefined()
    await expect(spool.append(Uint8Array.of(0x61))).rejects.toMatchObject({
      name: 'ZipSpoolBudgetExceededError',
      budget: 'zip-central-directory-bytes',
      limit: 3n,
      attempted: 4n,
    })
    await spool.clear()
  })

  it('does not allow callers to raise the product operation ceilings', () => {
    expect(() => new IndexedDbZipCentralDirectorySpool({
      maxEntries: ZIP_SPOOL_MAXIMUM_ENTRIES + 1,
    })).toThrow(/entry budget exceeds/u)
    expect(() => new IndexedDbZipCentralDirectorySpool({
      maxBytes: ZIP_SPOOL_MAXIMUM_BYTES + 1,
    })).toThrow(/byte budget exceeds/u)
  })

  it('keeps the per-record chunk bound independent from total budgets', async () => {
    const spool = new IndexedDbZipCentralDirectorySpool({
      namespace: 'chunk-budget',
      token: 'chunk-budget-token',
    })
    await expect(spool.append(new Uint8Array(ZIP_SPOOL_CHUNK_BYTES + 1))).rejects.toThrow(
      /durable chunk bound/u,
    )
    await spool.clear()
  })
})
