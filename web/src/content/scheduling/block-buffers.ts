import { MAXIMUM_CHUNK_BYTES } from '../../catalog/records'
import type { BlockRecord } from '../records'
import type { ContentPriority } from './selection'

const PREVIEW_RESERVATION_BYTES = 2 * MAXIMUM_CHUNK_BYTES

export interface BlockLoadBuffers {
  available(priority: ContentPriority): boolean
  acquireAttempt(priority: ContentPriority): (() => void) | undefined
  release(): void
}

/** Active results and attempts own bytes; idle cache only borrows unreserved space. */
export class BlockBufferPool {
  readonly #maximumBytes: number
  readonly #maximumRetainedCacheBytes: number
  readonly #previewBytes: number
  readonly #cache = new Map<string, BlockRecord>()
  readonly #listeners = new Set<() => void>()
  #reservedBytes = 0
  #cachedBytes = 0

  constructor(maximumBytes: number, maximumRetainedCacheBytes: number) {
    if (!Number.isSafeInteger(maximumBytes) || maximumBytes <= 0) {
      throw new RangeError('Block buffer capacity must be a positive safe integer')
    }
    if (!Number.isSafeInteger(maximumRetainedCacheBytes) || maximumRetainedCacheBytes < 0) {
      throw new RangeError('Retained block cache limit must be a non-negative safe integer')
    }
    this.#maximumBytes = maximumBytes
    this.#maximumRetainedCacheBytes = maximumRetainedCacheBytes
    this.#previewBytes = Math.min(PREVIEW_RESERVATION_BYTES, Math.floor(maximumBytes / 2))
  }

  createLoad(bytes: number): BlockLoadBuffers {
    // Validate the whole initial ownership claim before publishing queued work.
    const initialBytes = bytes * 2
    if (!Number.isSafeInteger(bytes) || bytes <= 0 || !Number.isSafeInteger(initialBytes) ||
        initialBytes > this.#maximumBytes) {
      throw new RangeError(`Block transfer needs ${initialBytes} reserved bytes; block buffer capacity is ${this.#maximumBytes}`)
    }
    let releaseShared: (() => void) | undefined
    let retired = false
    const needed = () => releaseShared === undefined ? initialBytes : bytes
    return {
      available: priority => !retired && this.#available(needed(), priority),
      acquireAttempt: priority => {
        if (retired || !this.#available(needed(), priority)) return undefined
        // Admission is atomic: checking the pair and reserving its halves
        // separately can reject the second half at the preview boundary.
        this.#reservedBytes += needed()
        releaseShared ??= this.#releaseOnce(bytes)
        this.#trimCache(this.#maximumBytes - this.#reservedBytes)
        return this.#releaseOnce(bytes)
      },
      release: () => {
        if (retired) return
        retired = true
        releaseShared?.()
      },
    }
  }

  has(key: string): boolean { return this.#cache.has(key) }

  get(key: string): BlockRecord | undefined {
    const record = this.#cache.get(key)
    if (record !== undefined) {
      this.#cache.delete(key)
      this.#cache.set(key, record)
    }
    return record
  }

  retain(key: string, record: BlockRecord): void {
    const ceiling = Math.min(this.#maximumRetainedCacheBytes, this.#maximumBytes - this.#reservedBytes)
    const bytes = record.data.byteLength
    if (bytes > ceiling) return
    this.#delete(key)
    this.#trimCache(ceiling - bytes)
    this.#cache.set(key, record)
    this.#cachedBytes += bytes
  }

  invalidate(prefix: string): void {
    for (const key of this.#cache.keys()) {
      if (key.startsWith(prefix)) this.#delete(key)
    }
  }

  clearCache(): void {
    this.#cache.clear()
    this.#cachedBytes = 0
  }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  #available(bytes: number, priority: ContentPriority): boolean {
    const ceiling = priority === 'preview' ? this.#maximumBytes
      : Math.max(bytes, this.#maximumBytes - this.#previewBytes)
    return this.#reservedBytes + bytes <= ceiling
  }

  #releaseOnce(bytes: number): () => void {
    let released = false
    return () => {
      if (released) return
      released = true
      this.#reservedBytes -= bytes
      for (const listener of this.#listeners) listener()
    }
  }

  #trimCache(ceiling: number): void {
    while (this.#cachedBytes > ceiling) {
      const oldest = this.#cache.keys().next()
      if (oldest.done) break
      this.#delete(oldest.value)
    }
  }

  #delete(key: string): void {
    const record = this.#cache.get(key)
    if (record === undefined) return
    this.#cache.delete(key)
    this.#cachedBytes -= record.data.byteLength
  }
}
