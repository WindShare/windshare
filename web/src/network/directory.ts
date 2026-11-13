import { BrowserDirectoryCache, MemoryDirectoryCache, type DirectoryCache } from './directory-cache'
import { MAXIMUM_ENVELOPE_BYTES, rejectRollback, usableDirectory, verifyDirectory, type NetworkRoot, type NetworkSnapshot } from './node-directory'

const REFRESH_TIMEOUT_MS = 6_000
const MIRROR_TIMEOUT_MS = 2_000
const REFRESH_INTERVAL_MS = 6 * 60 * 60 * 1000
const MINIMUM_BACKOFF_MS = 30_000
const MAXIMUM_BACKOFF_MS = 30 * 60 * 1000
const MIRRORS_PER_REFRESH = 3

export interface DirectoryObservation {
  readonly operationId: string
  readonly transition: 'accepted' | 'rejected'
  readonly mirror: string
  readonly generation?: number
  readonly serial?: number
  readonly trustedUntil?: number
  readonly failure?: unknown
}
export interface NetworkDirectoryOptions {
  readonly network: string
  readonly roots: readonly NetworkRoot[]
  readonly embedded?: string
  readonly mirrors: readonly string[]
  readonly origin?: string
  readonly cache?: DirectoryCache
  readonly now?: () => number
  readonly fetch?: typeof fetch
  readonly random?: () => number
  readonly observe?: (value: DirectoryObservation) => void
}

export class NetworkDirectory {
  readonly #options: NetworkDirectoryOptions
  readonly #cache: DirectoryCache
  readonly #now: () => number
  readonly #lifetime = new AbortController()
  readonly ready: Promise<void>
  #snapshot: NetworkSnapshot | undefined
  #pending: Promise<void> | undefined
  #timer: ReturnType<typeof setTimeout> | undefined
  #retryAt = 0
  #failures = 0
  #refreshSequence = 0

  constructor(options: NetworkDirectoryOptions) {
    if (options.mirrors.length > 8) throw new TypeError('Too many network mirrors')
    this.#options = options
    this.#cache = options.cache ?? (typeof indexedDB === 'undefined' ? new MemoryDirectoryCache() : new BrowserDirectoryCache())
    this.#now = options.now ?? Date.now
    this.ready = this.#initialize()
    this.ready.then(() => this.refresh()).catch(() => undefined)
  }

  get snapshot(): NetworkSnapshot | undefined { return usableDirectory(this.#snapshot, this.#now()) ? this.#snapshot : undefined }

  async #initialize(): Promise<void> {
    const record = await this.#cache.load()
    if (record) {
      const cached = await verifyDirectory(record.envelope, this.#options.network, this.#options.roots)
      if (cached.cursor.generation !== record.cursor.generation || cached.cursor.serial !== record.cursor.serial || cached.cursor.digest !== record.cursor.digest) throw new Error('Network cache history mismatch')
      this.#snapshot = cached
    }
    if (this.#options.embedded) {
      const embedded = await verifyDirectory(this.#options.embedded, this.#options.network, this.#options.roots)
      const current = this.#snapshot?.cursor
      if (!current || embedded.cursor.generation > current.generation || embedded.cursor.generation === current.generation && embedded.cursor.serial >= current.serial) {
        await this.#cache.commit(embedded)
        this.#snapshot = embedded
      }
    }
  }

  async refresh(): Promise<void> {
    await this.ready
    this.#lifetime.signal.throwIfAborted()
    if (this.#pending) return this.#pending
    if (this.#failures > 0 && this.#now() < this.#retryAt) throw new Error('Network refresh is in backoff')
    this.#pending = this.#refresh().finally(() => { this.#pending = undefined })
    return this.#pending
  }

  async #refresh(): Promise<void> {
    const operationId = `directory-refresh-${++this.#refreshSequence}`
    const overall = AbortSignal.any([this.#lifetime.signal, AbortSignal.timeout(REFRESH_TIMEOUT_MS)])
    const mirrors = [...this.#options.mirrors]
    const random = this.#options.random ?? Math.random
    for (let i = mirrors.length - 1; i > 0; i--) { const j = Math.floor(random() * (i + 1)); [mirrors[i], mirrors[j]] = [mirrors[j]!, mirrors[i]!] }
    if (this.#options.origin?.startsWith('https://')) mirrors.unshift(new URL('/.well-known/windshare-network', this.#options.origin).href)
    let last: unknown = new Error('Network temporarily unavailable')
    if (this.#options.roots.length > 0) for (const mirror of [...new Set(mirrors)].slice(0, MIRRORS_PER_REFRESH)) {
      try {
        const signal = AbortSignal.any([overall, AbortSignal.timeout(MIRROR_TIMEOUT_MS)])
        const data = await fetchEnvelope(mirror, signal, this.#options.fetch ?? fetch)
        const next = await verifyDirectory(data, this.#options.network, this.#options.roots)
        overall.throwIfAborted()
        if (!usableDirectory(next, this.#now())) throw new Error('Network directory expired')
        rejectRollback(this.#snapshot?.cursor, next.cursor)
        await this.#cache.commit(next)
        this.#snapshot = next
        this.#failures = 0
        this.#schedule(REFRESH_INTERVAL_MS)
        this.#observe({ operationId, transition: 'accepted', mirror, generation: next.cursor.generation, serial: next.cursor.serial, trustedUntil: next.trustedUntil })
        return
      } catch (failure) {
        last = failure
        this.#observe({ operationId, transition: 'rejected', mirror, failure })
        if (overall.aborted) break
      }
    }
    this.#failures++
    const backoff = Math.min(MAXIMUM_BACKOFF_MS, MINIMUM_BACKOFF_MS * 2 ** Math.min(this.#failures - 1, 10))
    this.#retryAt = this.#now() + backoff
    this.#schedule(backoff)
    throw last
  }

  #observe(event: DirectoryObservation): void {
    try { this.#options.observe?.(event) } catch { /* Diagnostics cannot revoke a committed trust update. */ }
  }

  #schedule(delay: number): void {
    if (this.#timer !== undefined) clearTimeout(this.#timer)
    if (!this.#lifetime.signal.aborted) this.#timer = setTimeout(() => { void this.refresh().catch(() => undefined) }, delay)
  }

  close(): void {
    this.#lifetime.abort()
    if (this.#timer !== undefined) clearTimeout(this.#timer)
  }
}

async function fetchEnvelope(mirror: string, signal: AbortSignal, request: typeof fetch): Promise<string> {
  if (new URL(mirror).protocol !== 'https:') throw new TypeError('Invalid network mirror')
  const response = await request(mirror, { signal, credentials: 'omit', cache: 'no-store', referrerPolicy: 'no-referrer' })
  if (!response.ok || response.url && new URL(response.url).protocol !== 'https:' || !response.body) throw new Error('Network mirror unavailable')
  const reader = response.body.getReader(), chunks: Uint8Array[] = []
  let size = 0
  try {
    for (;;) {
      const next = await reader.read()
      if (next.done) break
      size += next.value.byteLength
      if (size > MAXIMUM_ENVELOPE_BYTES) throw new Error('Network mirror response too large')
      chunks.push(next.value)
    }
  } finally { await reader.cancel().catch(() => undefined); reader.releaseLock() }
  const result = new Uint8Array(size)
  let offset = 0
  for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength }
  return new TextDecoder('utf-8', { fatal: true }).decode(result)
}
