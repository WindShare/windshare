import { concatBytes } from '../../crypto/bytes'
import { BrowserBlockCache, MemoryBlockCache, type BlockCache } from './cache'
import { freshBlocks, MAX_BLOCK_ENVELOPE_BYTES, rejectBlockRollback, verifyBlocks, type BlockCursor, type BlockSnapshot } from './blocks'

const REFRESH_INTERVAL_MS = 60_000
const REFRESH_TIMEOUT_MS = 8_000
export interface AuthorityObservation {
  readonly operationId: string; readonly transition: 'accepted' | 'rejected'; readonly endpoint: string
  readonly generation?: number; readonly version?: number; readonly failure?: unknown
}
export interface AuthorityConfig { readonly url: string; readonly root: string }
interface FeedOptions extends AuthorityConfig {
  readonly cache?: BlockCache; readonly request?: typeof fetch; readonly now?: () => number
  readonly observe?: (event: AuthorityObservation) => void
}
export class AuthorityBlockFeed {
  readonly #options: FeedOptions
  readonly #cache: BlockCache
  readonly #lifetime = new AbortController()
  readonly #listeners = new Set<() => void>()
  #ready: Promise<void> | undefined
  #snapshot: BlockSnapshot | undefined
  #pendingCursor: BlockCursor | undefined
  readonly #pendingDenials = new Set<string>()
  #timer: ReturnType<typeof setTimeout> | undefined
  #pending: Promise<void> | undefined
  #refreshAt = 0
  #sequence = 0

  constructor(options: FeedOptions) {
    const url = new URL(options.url)
    if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash || url.hostname.endsWith('.workers.dev')) throw new TypeError('Invalid authority endpoint')
    this.#options = options
    this.#cache = options.cache ?? (!('indexedDB' in globalThis) ? new MemoryBlockCache() : new BrowserBlockCache(options.root))
  }
  get ready(): Promise<void> {
    this.#ready ??= this.#load().catch(failure => this.#observe({ operationId: 'authority-cache', transition: 'rejected', endpoint: this.#options.url, failure }))
    return this.#ready
  }
  blocked(share: string): boolean { return this.#pendingDenials.has(share) || (this.#snapshot?.shares.has(share) ?? false) }
  observe(listener: () => void): () => void {
    this.#listeners.add(listener)
    this.#schedule()
    return () => {
      this.#listeners.delete(listener)
      if (!this.#listeners.size) this.#clearTimer()
    }
  }
  async #load(): Promise<void> {
    const cached = await this.#cache.load()
    if (!cached) return
    const verified = await verifyBlocks(cached.envelope, this.#options.root)
    rejectBlockRollback(cached.cursor, verified.cursor)
    rejectBlockRollback(verified.cursor, cached.cursor)
    this.#adopt(verified)
  }
  #adopt(snapshot: BlockSnapshot): void {
    rejectBlockRollback(this.#snapshot?.cursor, snapshot.cursor)
    rejectBlockRollback(this.#pendingCursor, snapshot.cursor)
    this.#snapshot = snapshot
    this.#pendingCursor = undefined
    this.#pendingDenials.clear()
    if (!this.#lifetime.signal.aborted) for (const listener of this.#listeners) listener()
  }
  refresh(): Promise<void> {
    if (!this.#pending) {
      this.#clearTimer()
      // A refresh started by an official dial can finish even if cached policy rejects that dial.
      this.#pending = this.#refresh().finally(() => {
        this.#pending = undefined
        this.#refreshAt = performance.now() + REFRESH_INTERVAL_MS
        this.#schedule()
      })
    }
    return this.#pending
  }
  #schedule(): void {
    if (this.#lifetime.signal.aborted || !this.#listeners.size || this.#pending || this.#timer !== undefined) return
    // Keep the same cadence across quick reconnects and losing parallel dials.
    const delay = this.#refreshAt - performance.now()
    if (delay <= 0) void this.refresh()
    else this.#timer = setTimeout(() => { void this.refresh() }, delay)
  }
  #clearTimer(): void {
    if (this.#timer === undefined) return
    clearTimeout(this.#timer)
    this.#timer = undefined
  }
  async #refresh(): Promise<void> {
    await this.ready
    if (this.#lifetime.signal.aborted) return
    const operationId = `authority-blocks-${++this.#sequence}`
    const mirror = new URL('/v1/blocks', this.#options.url).href
    const signal = AbortSignal.any([this.#lifetime.signal, AbortSignal.timeout(REFRESH_TIMEOUT_MS)])
    try {
      const response = await (this.#options.request ?? fetch)(mirror, { signal, redirect: 'error', credentials: 'omit', referrerPolicy: 'no-referrer', cache: 'no-store' })
      if (!response.ok || !response.body) throw new Error('Authority policy unavailable')
      const reader = response.body.getReader(), chunks: Uint8Array[] = []
      let size = 0
      try {
        for (;;) {
          const chunk = await reader.read()
          if (chunk.done) break
          size += chunk.value.length
          if (size > MAX_BLOCK_ENVELOPE_BYTES) throw new Error('Authority policy too large')
          chunks.push(chunk.value)
        }
      } finally { await reader.cancel().catch(() => undefined); reader.releaseLock() }
      const next = await verifyBlocks(new TextDecoder('utf-8', { fatal: true }).decode(concatBytes(chunks)), this.#options.root)
      signal.throwIfAborted()
      if (!freshBlocks(next, (this.#options.now ?? Date.now)())) throw new Error('Authority policy expired')
      rejectBlockRollback(this.#snapshot?.cursor, next.cursor)
      rejectBlockRollback(this.#pendingCursor, next.cursor)
      // Denials survive failed writes, but unblocking must cross the shared cache's
      // atomic revision barrier so an older response cannot bypass another tab.
      this.#pendingCursor = next.cursor
      for (const share of next.shares) this.#pendingDenials.add(share)
      for (const listener of this.#listeners) listener()
      await this.#cache.commit(next)
      this.#adopt(next)
      this.#observe({ operationId, transition: 'accepted', endpoint: mirror, generation: next.cursor.generation, version: next.cursor.version })
    } catch (failure) {
      this.#observe({ operationId, transition: 'rejected', endpoint: mirror, failure })
      // Another tab may have committed a newer revision during our request.
      await this.#load().catch(() => undefined)
    }
  }
  #observe(event: AuthorityObservation): void {
    try { this.#options.observe?.(event) } catch { /* Diagnostics do not own policy. */ }
  }
  close(): void {
    this.#lifetime.abort()
    this.#clearTimer()
    this.#listeners.clear()
  }
}
