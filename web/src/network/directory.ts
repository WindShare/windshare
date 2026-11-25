import { operationDeadlineSignal } from '../contracts/deadline'
import { BrowserDirectoryCache, sameCursor, type DirectoryCache } from './directory-cache'
import { MAXIMUM_ENVELOPE_BYTES, rejectRollback, usableDirectory, verifyDirectory, type DirectoryCursor, type NetworkRoot, type NetworkSnapshot } from './contracts/node-directory'

const REFRESH_TIMEOUT_MS = 6_000
const MIRROR_TIMEOUT_MS = 2_000
const REFRESH_INTERVAL_MS = 6 * 60 * 60 * 1000
const MINIMUM_BACKOFF_MS = 30_000
const MAXIMUM_BACKOFF_MS = 30 * 60 * 1000
const MAXIMUM_MIRRORS = 8
const MIRRORS_PER_REFRESH = 3

export type DirectoryMirrorTier = 'live' | 'fallback'

export type DirectoryObservation = {
  readonly operationId: string
  readonly transition: 'accepted' | 'rejected'
  readonly mirror: string
  readonly tier: DirectoryMirrorTier
  readonly generation?: number
  readonly serial?: number
  readonly trustedUntil?: number
  readonly failure?: unknown
} | {
  readonly operationId: string
  readonly transition: 'storage_fallback'
  readonly failure: DOMException
}
export interface DirectoryRefreshOptions {
  readonly explicit?: boolean
}

export interface NetworkDirectoryOptions {
  readonly network: string
  readonly roots: readonly NetworkRoot[]
  readonly embedded?: string
  readonly mirrors: readonly string[]
  readonly fallbackMirrors?: readonly string[]
  readonly origin?: string
  readonly cache?: DirectoryCache
  readonly now?: () => number
  readonly monotonicNow?: () => number
  readonly fetch?: typeof fetch
  readonly random?: () => number
  readonly observe?: (value: DirectoryObservation) => void
}

export class NetworkDirectory {
  readonly #options: NetworkDirectoryOptions
  readonly #initialized: Promise<DirectoryCache>
  readonly #now: () => number
  readonly #monotonicNow: () => number
  readonly #lifetime = new AbortController()
  readonly #snapshotListeners = new Set<() => void>()
  readonly #networkAvailable = () => {
    if (this.#lifetime.signal.aborted) return
    this.#networkGeneration += 1
    if (this.#failures === 0) return
    // Network connectivity returned; stale offline backoff must not stall recovery.
    this.#failures = 0
    this.#retryAt = 0
    if (this.#timer !== undefined) {
      clearTimeout(this.#timer)
      this.#timer = undefined
    }
    if (!this.#pending) void this.refresh().catch(() => undefined)
  }
  readonly ready: Promise<void>
  #snapshot: NetworkSnapshot | undefined
  #pending: Promise<void> | undefined
  readonly #targets = new Set<string>()
  #timer: ReturnType<typeof setTimeout> | undefined
  #retryAt = 0
  #failures = 0
  #networkGeneration = 0
  #refreshSequence = 0

  constructor(options: NetworkDirectoryOptions) {
    if (options.mirrors.length + (options.fallbackMirrors?.length ?? 0) > MAXIMUM_MIRRORS) {
      throw new TypeError('Too many network mirrors')
    }
    this.#options = options
    this.#now = options.now ?? Date.now
    this.#monotonicNow = options.monotonicNow ?? (() => performance.now())
    this.#initialized = this.#initialize()
    this.ready = this.#initialized.then(() => undefined)
    globalThis.addEventListener?.('online', this.#networkAvailable)
    this.ready.then(() => this.refresh()).catch(() => undefined)
  }

  get snapshot(): NetworkSnapshot | undefined { return usableDirectory(this.#snapshot, this.#now()) ? this.#snapshot : undefined }

  observeSnapshot(listener: () => void): () => void {
    this.#snapshotListeners.add(listener)
    return () => { this.#snapshotListeners.delete(listener) }
  }

  async #initialize(): Promise<DirectoryCache> {
    const provided = this.#options.cache
    const { cache, record, storageDenial } = provided
      ? { cache: provided, record: await provided.load(), storageDenial: undefined }
      : await BrowserDirectoryCache.initialize(this.#options.network)
    if (storageDenial) this.#observe({ operationId: 'directory-initialize', transition: 'storage_fallback', failure: storageDenial })
    let snapshot: NetworkSnapshot | undefined
    if (record) {
      const cached = await verifyDirectory(record.envelope, this.#options.network, this.#options.roots)
      if (cached.cursor.generation !== record.cursor.generation || cached.cursor.serial !== record.cursor.serial || cached.cursor.digest !== record.cursor.digest) throw new Error('Network cache history mismatch')
      snapshot = cached
    }
    try {
      if (this.#options.embedded) {
        const embedded = await verifyDirectory(this.#options.embedded, this.#options.network, this.#options.roots)
        const current = snapshot?.cursor
        if (!current || embedded.cursor.generation > current.generation || embedded.cursor.generation === current.generation && embedded.cursor.serial >= current.serial) {
          await cache.commit(embedded)
          snapshot = embedded
        }
      }
    } finally {
      // Consumers keep confirmed membership for an entire connection. Publish
      // once the release seed has been checked against the durable snapshot.
      if (snapshot) this.#publishSnapshot(snapshot)
    }
    return cache
  }

  async refresh(aliases: readonly string[] = [], options: DirectoryRefreshOptions = {}): Promise<void> {
    const cache = await this.#initialized
    this.#lifetime.signal.throwIfAborted()
    if (!this.#pending && !options.explicit && this.#failures > 0 && this.#monotonicNow() < this.#retryAt) {
      throw new Error('Network refresh is in backoff')
    }
    for (const alias of aliases) this.#targets.add(alias)
    this.#pending ??= Promise.resolve().then(() => this.#refresh(cache))
    return this.#pending
  }

  async #refresh(cache: DirectoryCache): Promise<void> {
    const operationId = `directory-refresh-${++this.#refreshSequence}`
    const networkGeneration = this.#networkGeneration
    let deadline: ReturnType<typeof operationDeadlineSignal> | undefined
    try {
      deadline = operationDeadlineSignal(this.#lifetime.signal, REFRESH_TIMEOUT_MS, new DOMException('Directory refresh timed out', 'TimeoutError'))
      const overall = deadline.signal
      const live = orderedLiveMirrors(this.#options)
      let accepted = false
      let committed: DirectoryCursor | undefined
      let attempts = 0
      let last: unknown = new Error('Network temporarily unavailable')
      const tryTier = async (mirrors: readonly string[], tier: DirectoryMirrorTier): Promise<boolean> => {
        for (const mirror of mirrors) {
          if (attempts >= MIRRORS_PER_REFRESH || overall.aborted) break
          attempts++
          try {
            const next = await this.#readMirror(cache, operationId, mirror, tier, overall, committed)
            accepted = true
            committed = next.cursor
            if ([...this.#targets].every(alias => next.nodes.some(node => node.alias === alias))) return true
          } catch (failure) {
            last = failure
            this.#observe({ operationId, transition: 'rejected', mirror, tier, failure })
            if (overall.aborted) break
          }
        }
        return false
      }
      if (this.#options.roots.length > 0) {
        const resolved = await tryTier(live, 'live')
        // Cold fallback mirrors carry static release snapshots and cannot advertise
        // newer aliases than a reachable live mirror; use them only when all live
        // mirrors fail.
        if (!resolved && !accepted && !overall.aborted) {
          await tryTier(orderedFallbackMirrors(this.#options, live), 'fallback')
        }
      }
      if (accepted) return
      if (networkGeneration !== this.#networkGeneration) {
        // Connectivity changed while this pass was in flight; do not lock the new
        // network behind stale offline backoff.
        this.#failures = 0
        this.#retryAt = 0
        this.#schedule(0)
        throw last
      }
      this.#failures++
      const backoff = Math.min(MAXIMUM_BACKOFF_MS, MINIMUM_BACKOFF_MS * 2 ** Math.min(this.#failures - 1, 10))
      this.#retryAt = this.#monotonicNow() + backoff
      this.#schedule(backoff)
      throw last
    } finally {
      deadline?.close()
      this.#targets.clear()
      this.#pending = undefined
    }
  }

  async #readMirror(cache: DirectoryCache, operationId: string, mirror: string, tier: DirectoryMirrorTier, overall: AbortSignal, committed?: DirectoryCursor): Promise<NetworkSnapshot> {
    const deadline = operationDeadlineSignal(overall, MIRROR_TIMEOUT_MS, new DOMException('Directory mirror timed out', 'TimeoutError'))
    let data: string
    try { data = await fetchEnvelope(mirror, deadline.signal, this.#options.fetch ?? fetch) }
    finally { deadline.close() }
    const next = await verifyDirectory(data, this.#options.network, this.#options.roots)
    overall.throwIfAborted()
    if (!usableDirectory(next, this.#now())) throw new Error('Network directory expired')
    rejectRollback(this.#snapshot?.cursor, next.cursor)
    if (!sameCursor(committed, next.cursor)) await cache.commit(next)
    this.#accept(next, operationId, mirror, tier)
    return next
  }

  #accept(snapshot: NetworkSnapshot, operationId: string, mirror: string, tier: DirectoryMirrorTier): void {
    const changed = !sameCursor(this.#snapshot?.cursor, snapshot.cursor)
    this.#failures = 0
    this.#retryAt = 0
    this.#schedule(REFRESH_INTERVAL_MS)
    if (changed) this.#publishSnapshot(snapshot)
    this.#observe({ operationId, transition: 'accepted', mirror, tier, generation: snapshot.cursor.generation, serial: snapshot.cursor.serial, trustedUntil: snapshot.trustedUntil })
  }

  #publishSnapshot(snapshot: NetworkSnapshot): void {
    this.#snapshot = snapshot
    for (const listener of this.#snapshotListeners) {
      try { listener() } catch { /* Consumers cannot revoke a committed directory. */ }
    }
  }

  #observe(event: DirectoryObservation): void {
    try { this.#options.observe?.(event) } catch { /* Diagnostics cannot revoke a committed trust update. */ }
  }

  #schedule(delay: number): void {
    if (this.#timer !== undefined) clearTimeout(this.#timer)
    if (this.#lifetime.signal.aborted) {
      this.#timer = undefined
      return
    }
    this.#timer = setTimeout(() => {
      this.#timer = undefined
      if (this.#lifetime.signal.aborted || this.#pending) return
      if (this.#failures > 0) {
        // Timers can wake slightly ahead of performance.now(); reschedule the
        // remainder instead of letting the backoff guard kill background retries.
        const remaining = this.#retryAt - this.#monotonicNow()
        if (remaining > 0) {
          this.#schedule(remaining)
          return
        }
      }
      void this.refresh().catch(() => undefined)
    }, Math.max(0, Math.ceil(delay)))
  }

  close(): void {
    this.#lifetime.abort()
    globalThis.removeEventListener?.('online', this.#networkAvailable)
    if (this.#timer !== undefined) {
      clearTimeout(this.#timer)
      this.#timer = undefined
    }
    this.#snapshotListeners.clear()
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

function parseMirror(raw: string): URL | undefined {
  try { return new URL(raw) } catch { return undefined }
}

function shuffledMirrors(raw: readonly string[], random: () => number): string[] {
  const mirrors = [...raw]
  for (let i = mirrors.length - 1; i > 0; i--) {
    const j = Math.floor(random() * (i + 1))
    ;[mirrors[i], mirrors[j]] = [mirrors[j]!, mirrors[i]!]
  }
  return mirrors
}

function orderedLiveMirrors(options: NetworkDirectoryOptions): string[] {
  const mirrors = shuffledMirrors(options.mirrors, options.random ?? Math.random)
  const origin = parseMirror(options.origin ?? '')
  if (origin?.protocol === 'https:') {
    const sameOrigin = new URL('/.well-known/windshare-network', origin).href
    // Only prioritize the current origin when it is already a configured live mirror so
    // self-hosted frontends keep all built-in mirrors within MIRRORS_PER_REFRESH.
    const index = mirrors.findIndex(mirror => parseMirror(mirror)?.href === sameOrigin)
    if (index !== -1) {
      mirrors.splice(index, 1)
      mirrors.unshift(sameOrigin)
    }
  }
  return [...new Set(mirrors)]
}

function orderedFallbackMirrors(options: NetworkDirectoryOptions, live: readonly string[]): string[] {
  if (!options.fallbackMirrors?.length) return []
  const seen = new Set(live)
  return [...new Set(shuffledMirrors(options.fallbackMirrors, options.random ?? Math.random))]
    .filter(mirror => !seen.has(mirror))
}
