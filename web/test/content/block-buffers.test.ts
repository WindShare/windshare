import { describe, expect, it } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { BlockBufferPool } from '../../src/content/scheduling/block-buffers'
import type { V2BlockRecord } from '../../src/content/v2-records'

const BLOCK_BYTES = 1024
function record(bytes = BLOCK_BYTES): V2BlockRecord {
  return {
    descriptor: {
      shareInstance: new Uint8Array(16), shareInstanceId: 'share',
      fileId: new Uint8Array(16), fileIdText: 'file',
      fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
      exactSize: BigInt(bytes), geometry: new FileGeometry(BigInt(bytes), BigInt(bytes)),
    },
    localBlockIndex: 0n, data: new Uint8Array(bytes),
  }
}

describe('block buffer ownership', () => {
  it('admits a complete result/attempt pair atomically at the minimum capacity', () => {
    const pool = new BlockBufferPool(2 * BLOCK_BYTES, 0)
    const load = pool.createLoad(BLOCK_BYTES)
    expect(load.available('download')).toBe(true)
    const releaseAttempt = load.acquireAttempt('download')
    expect(releaseAttempt).toBeDefined()
    const next = pool.createLoad(BLOCK_BYTES)
    expect(next.available('download')).toBe(false)
    releaseAttempt!()
    expect(next.available('download')).toBe(false)
    load.release()
    expect(next.available('download')).toBe(true)
  })

  it('evicts least recently used cache to make room for active transfer ownership', () => {
    const pool = new BlockBufferPool(4 * BLOCK_BYTES, 4 * BLOCK_BYTES)
    for (const key of ['a', 'b', 'c', 'd']) pool.retain(key, record())
    expect(pool.get('a')).toBeDefined()
    const load = pool.createLoad(BLOCK_BYTES)
    const releaseAttempt = load.acquireAttempt('download')
    expect(releaseAttempt).toBeDefined()
    expect(pool.has('b')).toBe(false)
    expect(pool.has('c')).toBe(false)
    expect(pool.has('d')).toBe(true)
    expect(pool.has('a')).toBe(true)
    releaseAttempt!(); load.release()
  })

  it('keeps preview capacity while downloads occupy their share of the pool', () => {
    const pool = new BlockBufferPool(4 * BLOCK_BYTES, 0)
    const download = pool.createLoad(BLOCK_BYTES)
    const releaseDownload = download.acquireAttempt('download')!
    const preview = pool.createLoad(BLOCK_BYTES)
    expect(preview.available('download')).toBe(false)
    const releasePreview = preview.acquireAttempt('preview')
    expect(releasePreview).toBeDefined()
    releaseDownload(); download.release()
    releasePreview!(); preview.release()
  })

  it('retains attempt reservations after the shared result retires, with idempotent release', () => {
    const pool = new BlockBufferPool(8 * BLOCK_BYTES, 0)
    const load = pool.createLoad(BLOCK_BYTES)
    const primary = load.acquireAttempt('preview')!
    const rescue = load.acquireAttempt('preview')!
    const fullPool = pool.createLoad(4 * BLOCK_BYTES)
    primary(); primary()
    load.release(); load.release()
    expect(load.acquireAttempt('preview')).toBeUndefined()
    expect(fullPool.available('preview')).toBe(false)
    rescue(); rescue()
    expect(fullPool.available('preview')).toBe(true)
  })

  it('bounds retention independently, including replacement and invalidation', () => {
    const pool = new BlockBufferPool(4 * BLOCK_BYTES, BLOCK_BYTES)
    pool.retain('file:a', record())
    pool.retain('file:a', record())
    pool.retain('too-large', record(2 * BLOCK_BYTES))
    expect(pool.has('file:a')).toBe(true)
    expect(pool.has('too-large')).toBe(false)
    pool.invalidate('file:')
    expect(pool.has('file:a')).toBe(false)
    pool.retain('other', record())
    pool.clearCache()
    expect(pool.has('other')).toBe(false)
    pool.retain('after-clear', record())
    expect(pool.has('after-clear')).toBe(true)
  })

  it('rejects impossible working capacity and malformed limits explicitly', () => {
    expect(() => new BlockBufferPool(0, 0)).toThrow(RangeError)
    expect(() => new BlockBufferPool(BLOCK_BYTES, -1)).toThrow(RangeError)
    const pool = new BlockBufferPool(BLOCK_BYTES, 0)
    expect(() => pool.createLoad(BLOCK_BYTES)).toThrow('Block transfer needs 2048 reserved bytes; block buffer capacity is 1024')
  })
})
