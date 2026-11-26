import type { BrowserOutputWindow } from '../../output/backends/contract'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveTaskRecord } from '../../output/store/types'
import { classifyNativeError } from '../../transfer/fault'
import type { PendingRetainedAction, RetainedInventorySnapshot, RetainedTaskSnapshot } from './contracts'
import type { ReceiveStorage, ReceiveStoreAccess } from './receive-storage'
import {
  resolveRetainedCandidates,
  retainedSnapshot,
  retainedTaskEnvironment,
  type ResolvedRetainedCandidate,
} from './retained'

type InventoryLoadMode = 'foreground' | 'silent'

type ResolvedInventoryEntry = ResolvedRetainedCandidate

type MutationOverlayEntry =
  | Readonly<{ seq: number; kind: 'upsert'; snapshot: RetainedTaskSnapshot; updatedAt: number }>
  | Readonly<{ seq: number; kind: 'remove' }>

export interface RetainedTaskInventoryOptions {
  readonly storage: ReceiveStorage
  readonly windowPort: BrowserOutputWindow
  readonly opfsEnv?: OPFSEnvironment
  readonly outputTrace?: OutputTraceSource
  readonly signal: AbortSignal
  readonly isDisposed: () => boolean
  readonly isLocalTask: (taskId: string) => boolean
  readonly onChange: () => void
  readonly onAuthoritativeSync?: (changed: boolean) => void
}

/**
 * Single source of truth for the retained task inventory shown in Downloads and
 * matched against the workspace selection.
 *
 * Uses monotonic load epochs and a bounded in-flight mutation overlay so concurrent
 * scans never overwrite newer local settlements or deletions.
 */
export class RetainedTaskInventory {
  readonly #storage: ReceiveStorage
  readonly #windowPort: BrowserOutputWindow
  readonly #opfsEnv: OPFSEnvironment | undefined
  readonly #outputTrace: OutputTraceSource | undefined
  readonly #signal: AbortSignal
  readonly #isDisposed: () => boolean
  readonly #isLocalTask: (taskId: string) => boolean
  readonly #onChange: () => void
  readonly #onAuthoritativeSync: ((changed: boolean) => void) | undefined

  #fullKind: RetainedInventorySnapshot['kind'] = 'unloaded'
  #fullEntries = new Map<string, ResolvedInventoryEntry>()
  #fullTasksCache: readonly RetainedTaskSnapshot[] = Object.freeze([])
  #fullLoadEpoch = 0
  // Tracks task IDs present (or transiently unreadable) in the last completed IndexedDB scan
  // so workspace reconciliation only clears tasks confirmed deleted from the store.
  #authoritativeTaskIds: Set<string> | null = null

  #mutationSeq = 0
  #inFlightLoads = 0
  #mutationOverlay = new Map<string, MutationOverlayEntry>()

  constructor(options: RetainedTaskInventoryOptions) {
    this.#storage = options.storage
    this.#windowPort = options.windowPort
    this.#opfsEnv = options.opfsEnv
    this.#outputTrace = options.outputTrace
    this.#signal = options.signal
    this.#isDisposed = options.isDisposed
    this.#isLocalTask = options.isLocalTask
    this.#onChange = options.onChange
    this.#onAuthoritativeSync = options.onAuthoritativeSync
  }

  get tasks(): readonly RetainedTaskSnapshot[] {
    return this.#fullTasksCache
  }

  isConfirmedAbsent(taskId: string): boolean {
    return (this.#fullKind === 'available' || this.#fullKind === 'empty') &&
      this.#authoritativeTaskIds !== null &&
      !this.#authoritativeTaskIds.has(taskId)
  }

  snapshot(pending: PendingRetainedAction | null): RetainedInventorySnapshot {
    return Object.freeze({
      kind: this.#fullKind,
      tasks: this.#fullTasksCache,
      pending,
    })
  }

  loadAll(): Promise<void> {
    return this.#startLoad('foreground')
  }

  async ensureAll(): Promise<void> {
    if (this.#fullKind === 'unloaded' || this.#fullKind === 'failed') {
      await this.loadAll()
    }
  }

  /**
   * Reconciles the cached inventory against IndexedDB and cross-tab Web Locks.
   * Revalidates silently when a settled snapshot already exists so reopening Downloads
   * never flashes a loading state or clears existing tasks on transient errors.
   */
  refreshAll(): Promise<void> {
    const settled = this.#fullKind === 'available' || this.#fullKind === 'empty'
    return this.#startLoad(settled ? 'silent' : 'foreground')
  }

  #startLoad(mode: InventoryLoadMode): Promise<void> {
    if (this.#isDisposed()) return Promise.resolve()
    const epoch = ++this.#fullLoadEpoch
    const startSeq = this.#mutationSeq
    this.#inFlightLoads += 1
    return this.#storage.run(access => this.#loadFull(access, epoch, startSeq, mode))
      .finally(() => this.#finishInFlightLoad())
  }

  upsertRecord(record: ReceiveTaskRecord): void {
    const seq = ++this.#mutationSeq
    const snapshot = retainedSnapshot(record, false)
    const entry: ResolvedInventoryEntry = Object.freeze({ snapshot, updatedAt: record.updatedAt })
    if (this.#inFlightLoads > 0) {
      this.#mutationOverlay.set(record.taskId, Object.freeze({ seq, kind: 'upsert', snapshot, updatedAt: record.updatedAt }))
    }
    this.#authoritativeTaskIds?.add(record.taskId)
    if (this.#fullKind === 'available' || this.#fullKind === 'empty') {
      this.#fullEntries.set(record.taskId, entry)
      this.#fullKind = 'available'
      this.#rebuildFullTasksCache()
    }
  }

  removeTask(taskId: string): void {
    const seq = ++this.#mutationSeq
    if (this.#inFlightLoads > 0) {
      this.#mutationOverlay.set(taskId, Object.freeze({ seq, kind: 'remove' }))
    }
    if (this.#fullEntries.delete(taskId)) {
      if (this.#fullKind === 'available' && this.#fullEntries.size === 0) {
        this.#fullKind = 'empty'
      }
      this.#rebuildFullTasksCache()
    }
  }

  async #loadFull(
    access: ReceiveStoreAccess,
    epoch: number,
    startSeq: number,
    mode: InventoryLoadMode,
  ): Promise<void> {
    if (this.#isStaleLoad(epoch)) return
    await access.waitForTaskWrites()
    if (this.#isStaleLoad(epoch)) return
    const store = await access.openStoreOptional()
    if (this.#isStaleLoad(epoch)) return
    if (store === undefined) {
      this.#handleStoreUnavailable(startSeq, mode)
      return
    }
    if (mode === 'foreground') {
      this.#fullKind = 'loading'
      this.#onChange()
    }
    try {
      const env = retainedTaskEnvironment(store, this.#envPorts(), this.#signal)
      const records = await store.listTasks()
      if (this.#isStaleLoad(epoch)) return
      const { entries: resolved, unreadableTaskIds } = await resolveRetainedCandidates(env, records, this.#isLocalTask)
      if (this.#isStaleLoad(epoch)) return
      const entries = new Map(resolved.map(entry => [entry.snapshot.taskId, entry]))
      const changed = this.#commitFullEntries(entries, startSeq)
      this.#recordAuthoritativeScan(resolved, unreadableTaskIds, startSeq)
      if (this.#onAuthoritativeSync !== undefined) {
        this.#onAuthoritativeSync(changed)
      } else if (changed) {
        this.#onChange()
      }
    } catch (error) {
      if (this.#isStaleLoad(epoch)) return
      this.#handleLoadFailure(mode, error)
    }
  }

  #isStaleLoad(epoch: number): boolean {
    return this.#isDisposed() || epoch !== this.#fullLoadEpoch
  }

  #recordAuthoritativeScan(
    resolved: readonly ResolvedInventoryEntry[],
    unreadableTaskIds: ReadonlySet<string>,
    startSeq: number,
  ): void {
    const present = new Set<string>(unreadableTaskIds)
    for (const entry of resolved) present.add(entry.snapshot.taskId)
    for (const [taskId, op] of this.#mutationOverlay) {
      if (op.seq > startSeq && op.kind === 'upsert') present.add(taskId)
    }
    this.#authoritativeTaskIds = present
  }

  #handleStoreUnavailable(startSeq: number, mode: InventoryLoadMode): void {
    if (mode === 'silent') {
      if (this.#fullEntries.size > 0) {
        emitOutputTrace(this.#outputTrace, 'continuation', { transition: 'inventory_refresh_store_unavailable' })
      }
      return
    }
    this.#authoritativeTaskIds = null
    if (this.#commitFullEntries(new Map(), startSeq)) {
      this.#onChange()
    }
  }

  #handleLoadFailure(mode: InventoryLoadMode, error: unknown): void {
    if (mode === 'silent') {
      emitOutputTrace(this.#outputTrace, 'continuation', {
        transition: 'inventory_refresh_failed',
        error_class: classifyNativeError(error),
      })
      return
    }
    this.#authoritativeTaskIds = null
    this.#fullEntries = new Map()
    this.#fullTasksCache = Object.freeze([])
    this.#fullKind = 'failed'
    this.#onChange()
  }

  #commitFullEntries(entries: Map<string, ResolvedInventoryEntry>, startSeq: number): boolean {
    for (const [taskId, op] of this.#mutationOverlay) {
      if (op.seq <= startSeq) continue
      if (op.kind === 'remove') entries.delete(taskId)
      else entries.set(taskId, Object.freeze({ snapshot: op.snapshot, updatedAt: op.updatedAt }))
    }
    const nextKind: RetainedInventorySnapshot['kind'] = entries.size === 0 ? 'empty' : 'available'
    const orderedEntries = [...entries.values()].sort((left, right) => right.updatedAt - left.updatedAt)
    if (this.#matchesCurrent(nextKind, orderedEntries)) {
      return false
    }
    this.#fullEntries = entries
    this.#fullKind = nextKind
    this.#fullTasksCache = Object.freeze(orderedEntries.map(entry => entry.snapshot))
    return true
  }

  #matchesCurrent(
    nextKind: RetainedInventorySnapshot['kind'],
    orderedEntries: readonly ResolvedInventoryEntry[],
  ): boolean {
    if (this.#fullKind !== nextKind || this.#fullTasksCache.length !== orderedEntries.length) {
      return false
    }
    for (let index = 0; index < orderedEntries.length; index += 1) {
      const nextEntry = orderedEntries[index]!
      const currentSnapshot = this.#fullTasksCache[index]!
      const currentEntry = this.#fullEntries.get(currentSnapshot.taskId)
      if (
        currentEntry === undefined ||
        currentEntry.updatedAt !== nextEntry.updatedAt ||
        !equalInventoryValue(currentSnapshot, nextEntry.snapshot)
      ) {
        return false
      }
    }
    return true
  }

  #rebuildFullTasksCache(): void {
    const ordered = [...this.#fullEntries.values()]
      .sort((left, right) => right.updatedAt - left.updatedAt)
      .map(entry => entry.snapshot)
    this.#fullTasksCache = Object.freeze(ordered)
  }

  #finishInFlightLoad(): void {
    this.#inFlightLoads = Math.max(0, this.#inFlightLoads - 1)
    if (this.#inFlightLoads === 0 && this.#mutationOverlay.size > 0) {
      this.#mutationOverlay.clear()
    }
  }

  #envPorts() {
    return {
      windowPort: this.#windowPort,
      ...(this.#opfsEnv === undefined ? {} : { opfsEnv: this.#opfsEnv }),
      ...(this.#outputTrace === undefined ? {} : { outputTrace: this.#outputTrace }),
    }
  }
}

function equalInventoryValue(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true
  if (typeof left !== 'object' || typeof right !== 'object' || left === null || right === null) {
    return false
  }
  if (Array.isArray(left) || Array.isArray(right)) {
    return Array.isArray(left) && Array.isArray(right) && equalInventoryArrays(left, right)
  }
  return equalInventoryRecords(left as Record<string, unknown>, right as Record<string, unknown>)
}

function equalInventoryArrays(left: readonly unknown[], right: readonly unknown[]): boolean {
  return left.length === right.length && left.every((item, index) => equalInventoryValue(item, right[index]))
}

function equalInventoryRecords(
  left: Readonly<Record<string, unknown>>,
  right: Readonly<Record<string, unknown>>,
): boolean {
  const leftKeys = Object.keys(left)
  const rightKeys = Object.keys(right)
  if (leftKeys.length !== rightKeys.length) return false
  return leftKeys.every(key => Object.hasOwn(right, key) && equalInventoryValue(left[key], right[key]))
}
