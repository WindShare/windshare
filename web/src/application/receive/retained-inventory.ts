import type { BrowserOutputWindow } from '../../output/backends/contract'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveTaskRecord } from '../../output/store/types'
import { classifyNativeError } from '../../transfer/fault'
import type { PendingRetainedAction, RetainedInventorySnapshot, RetainedTaskSnapshot } from './contracts'
import type { ReceiveStorage, ReceiveStoreAccess } from './receive-storage'
import { resolveRetainedCandidates, retainedSnapshot, retainedTaskEnvironment } from './retained'
import { isSingleFileReuseCandidate } from './single-file'

type ResolvedInventoryEntry = Readonly<{
  snapshot: RetainedTaskSnapshot
  updatedAt: number
}>

type ShareIndexEntry =
  | Readonly<{ kind: 'raw'; record: ReceiveTaskRecord }>
  | Readonly<{ kind: 'resolved'; snapshot: RetainedTaskSnapshot; updatedAt: number }>

type ShareIndexState =
  | Readonly<{ kind: 'idle'; shareInstance: null }>
  | Readonly<{ kind: 'loading'; shareInstance: string; epoch: number; promise: Promise<void> }>
  | Readonly<{ kind: 'ready'; shareInstance: string; entries: Map<string, ShareIndexEntry> }>
  | Readonly<{ kind: 'failed'; shareInstance: string; entries: Map<string, ShareIndexEntry> }>

type MutationOverlayEntry =
  | Readonly<{ seq: number; kind: 'upsert'; snapshot: RetainedTaskSnapshot; updatedAt: number }>
  | Readonly<{ seq: number; kind: 'remove' }>

export type KeyCandidatesInspection =
  | Readonly<{ kind: 'none' }>
  | Readonly<{ kind: 'ready'; candidates: readonly RetainedTaskSnapshot[] }>
  | Readonly<{ kind: 'needs-resolution'; taskIds: readonly string[] }>

const IDLE_SHARE_INDEX: ShareIndexState = Object.freeze({ kind: 'idle', shareInstance: null })

export interface RetainedTaskInventoryOptions {
  readonly storage: ReceiveStorage
  readonly windowPort: BrowserOutputWindow
  readonly opfsEnv?: OPFSEnvironment
  readonly outputTrace?: OutputTraceSource
  readonly signal: AbortSignal
  readonly isDisposed: () => boolean
  readonly isLocalTask: (taskId: string) => boolean
  readonly onChange: () => void
}

/**
 * Single source of truth for retained task inventory across both the fast per-share
 * single-file lookup index and the full cross-share Downloads list.
 *
 * Uses monotonic load epochs and a bounded in-flight mutation overlay so concurrent
 * scans never overwrite newer local settlements/deletions, and writes normalized
 * candidate resolutions back into the index.
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

  #fullKind: RetainedInventorySnapshot['kind'] = 'unloaded'
  #fullEntries = new Map<string, ResolvedInventoryEntry>()
  #fullTasksCache: readonly RetainedTaskSnapshot[] = Object.freeze([])
  #fullLoadEpoch = 0

  #shareIndex: ShareIndexState = IDLE_SHARE_INDEX
  #shareLoadEpoch = 0

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
  }

  get tasks(): readonly RetainedTaskSnapshot[] {
    return this.#fullTasksCache
  }

  snapshot(pending: PendingRetainedAction | null): RetainedInventorySnapshot {
    return Object.freeze({
      kind: this.#fullKind,
      tasks: this.#fullTasksCache,
      pending,
    })
  }

  isShareLoading(shareInstance: string): boolean {
    return this.#shareIndex.kind === 'loading' && this.#shareIndex.shareInstance === shareInstance
  }

  pendingSharePromise(shareInstance: string): Promise<void> | undefined {
    return this.#shareIndex.kind === 'loading' && this.#shareIndex.shareInstance === shareInstance
      ? this.#shareIndex.promise
      : undefined
  }

  preloadShare(shareInstance: string, hasIndexedDb: boolean): void {
    if (this.#isDisposed()) return
    // Never reset a failed lookup back to loading on click; failed degrades to fresh download.
    if (this.#shareIndex.shareInstance === shareInstance) {
      return
    }
    if (!hasIndexedDb) {
      this.#shareIndex = Object.freeze({ kind: 'ready', shareInstance, entries: new Map() })
      return
    }
    const epoch = ++this.#shareLoadEpoch
    const startSeq = this.#mutationSeq
    let resolvePending!: () => void
    const promise = new Promise<void>(resolve => { resolvePending = resolve })
    this.#shareIndex = Object.freeze({ kind: 'loading', shareInstance, epoch, promise })
    this.#inFlightLoads += 1
    this.#onChange()

    void this.#storage.run(access => this.#loadShare(access, shareInstance, epoch, startSeq))
      .finally(() => {
        resolvePending()
        this.#finishInFlightLoad()
      })
  }

  resetShare(): void {
    this.#shareLoadEpoch += 1
    this.#shareIndex = IDLE_SHARE_INDEX
  }

  loadAll(): Promise<void> {
    if (this.#isDisposed()) return Promise.resolve()
    const epoch = ++this.#fullLoadEpoch
    const startSeq = this.#mutationSeq
    this.#inFlightLoads += 1
    return this.#storage.run(access => this.#loadFull(access, epoch, startSeq))
      .finally(() => this.#finishInFlightLoad())
  }

  async ensureAll(): Promise<void> {
    if (this.#fullKind === 'unloaded' || this.#fullKind === 'failed') {
      await this.loadAll()
    }
  }

  upsertRecord(record: ReceiveTaskRecord): void {
    const seq = ++this.#mutationSeq
    const snapshot = retainedSnapshot(record, false)
    const entry: ResolvedInventoryEntry = Object.freeze({ snapshot, updatedAt: record.updatedAt })
    if (this.#inFlightLoads > 0) {
      this.#mutationOverlay.set(record.taskId, Object.freeze({ seq, kind: 'upsert', snapshot, updatedAt: record.updatedAt }))
    }
    if (this.#fullKind === 'available' || this.#fullKind === 'empty') {
      this.#fullEntries.set(record.taskId, entry)
      this.#fullKind = 'available'
      this.#rebuildFullTasksCache()
    }
    this.#upsertIntoShareIndex(record.shareInstance, { kind: 'resolved', snapshot, updatedAt: record.updatedAt })
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
    if (this.#shareIndex.kind === 'ready' || this.#shareIndex.kind === 'failed') {
      this.#shareIndex.entries.delete(taskId)
    }
  }

  inspectKeyCandidates(shareInstance: string, key: string): KeyCandidatesInspection {
    const matching = this.#collectShareEntries(shareInstance, key)
    if (matching.length === 0) return Object.freeze({ kind: 'none' })

    const resolved: RetainedTaskSnapshot[] = []
    let needsResolution = false
    for (const entry of matching) {
      if (entryNeedsResolution(entry)) {
        needsResolution = true
      } else if (entry.kind === 'resolved') {
        resolved.push(entry.snapshot)
      }
    }
    if (needsResolution) {
      return Object.freeze({
        kind: 'needs-resolution',
        taskIds: Object.freeze(matching.map(entryTaskId)),
      })
    }
    return Object.freeze({ kind: 'ready', candidates: Object.freeze(resolved) })
  }

  async resolveKeyCandidates(
    shareInstance: string,
    key: string,
    taskIds: readonly string[],
  ): Promise<readonly RetainedTaskSnapshot[]> {
    if (taskIds.length === 0 || this.#isDisposed()) return Object.freeze([])
    let resolved: RetainedTaskSnapshot[] = []
    await this.#storage.run(async access => {
      await access.waitForTaskWrites()
      if (this.#isDisposed()) return
      const store = await access.openStoreOptional()
      if (this.#isDisposed()) return
      if (store === undefined) return
      const env = retainedTaskEnvironment(store, this.#envPorts(), this.#signal)
      const fetched: ReceiveTaskRecord[] = []
      const missingIds: string[] = []
      for (const taskId of taskIds) {
        this.#signal.throwIfAborted()
        const record = await store.getTask(taskId)
        if (record === undefined) {
          missingIds.push(taskId)
        } else {
          fetched.push(record)
        }
      }
      const normalized = await resolveRetainedCandidates(env, fetched, this.#isLocalTask)
      if (this.#isDisposed()) return

      const normalizedById = new Map(normalized.map(snapshot => [snapshot.taskId, snapshot]))
      const updatedAtById = new Map(fetched.map(record => [record.taskId, record.updatedAt]))
      for (const taskId of missingIds) {
        this.#pruneMissingCandidate(taskId)
      }
      for (const record of fetched) {
        const snapshot = normalizedById.get(record.taskId)
        if (snapshot === undefined) {
          this.#pruneMissingCandidate(record.taskId)
          continue
        }
        this.#writeResolvedSnapshot(snapshot, updatedAtById.get(record.taskId) ?? record.updatedAt)
      }
      resolved = normalized.filter(snapshot => snapshot.shareInstance === shareInstance && snapshot.selectionKey === key)
    })
    return Object.freeze(resolved)
  }

  async #loadShare(
    access: ReceiveStoreAccess,
    shareInstance: string,
    epoch: number,
    startSeq: number,
  ): Promise<void> {
    try {
      await access.waitForTaskWrites()
      if (this.#isDisposed() || epoch !== this.#shareLoadEpoch) return
      const store = await access.openStoreOptional()
      if (this.#isDisposed() || epoch !== this.#shareLoadEpoch) return
      const records = store === undefined ? Object.freeze([]) : await store.listTasksByShare(shareInstance)
      if (this.#isDisposed() || epoch !== this.#shareLoadEpoch) return
      const entries = this.#mergeShareScan(shareInstance, records, startSeq)
      this.#shareIndex = Object.freeze({ kind: 'ready', shareInstance, entries })
      this.#onChange()
    } catch (error) {
      if (this.#isDisposed() || epoch !== this.#shareLoadEpoch) return
      emitOutputTrace(this.#outputTrace, 'continuation', {
        share_instance: shareInstance,
        transition: 'share_index_preload_failed',
        error_class: classifyNativeError(error),
      })
      const entries = new Map<string, ShareIndexEntry>()
      this.#applyOverlayToShareEntries(entries, shareInstance, startSeq)
      this.#shareIndex = Object.freeze({ kind: 'failed', shareInstance, entries })
      this.#onChange()
    }
  }

  async #loadFull(access: ReceiveStoreAccess, epoch: number, startSeq: number): Promise<void> {
    if (this.#isDisposed() || epoch !== this.#fullLoadEpoch) return
    await access.waitForTaskWrites()
    if (this.#isDisposed() || epoch !== this.#fullLoadEpoch) return
    const store = await access.openStoreOptional()
    if (this.#isDisposed() || epoch !== this.#fullLoadEpoch) return
    if (store === undefined) {
      this.#commitFullEntries(new Map(), startSeq)
      this.#onChange()
      return
    }
    this.#fullKind = 'loading'
    this.#onChange()
    try {
      const env = retainedTaskEnvironment(store, this.#envPorts(), this.#signal)
      const records = await store.listTasks()
      if (this.#isDisposed() || epoch !== this.#fullLoadEpoch) return
      const snapshots = await resolveRetainedCandidates(env, records, this.#isLocalTask)
      if (this.#isDisposed() || epoch !== this.#fullLoadEpoch) return
      const updatedAtById = new Map(records.map(record => [record.taskId, record.updatedAt]))
      const entries = new Map<string, ResolvedInventoryEntry>()
      for (const snapshot of snapshots) {
        entries.set(snapshot.taskId, Object.freeze({
          snapshot,
          updatedAt: updatedAtById.get(snapshot.taskId) ?? snapshot.display?.createdAtMilliseconds ?? 0,
        }))
      }
      this.#commitFullEntries(entries, startSeq)
      this.#onChange()
    } catch {
      if (this.#isDisposed() || epoch !== this.#fullLoadEpoch) return
      this.#fullEntries = new Map()
      this.#fullTasksCache = Object.freeze([])
      this.#fullKind = 'failed'
      this.#onChange()
    }
  }

  #mergeShareScan(
    shareInstance: string,
    records: readonly ReceiveTaskRecord[],
    startSeq: number,
  ): Map<string, ShareIndexEntry> {
    const entries = new Map<string, ShareIndexEntry>()
    for (const record of records) {
      const existing = this.#fullEntries.get(record.taskId)
      if (
        existing !== undefined &&
        existing.snapshot.lifecycle.generation >= record.lifecycle.generation &&
        !existing.snapshot.activeElsewhere
      ) {
        entries.set(record.taskId, Object.freeze({
          kind: 'resolved',
          snapshot: existing.snapshot,
          updatedAt: existing.updatedAt,
        }))
      } else {
        entries.set(record.taskId, Object.freeze({ kind: 'raw', record }))
      }
    }
    this.#applyOverlayToShareEntries(entries, shareInstance, startSeq)
    return entries
  }

  #commitFullEntries(entries: Map<string, ResolvedInventoryEntry>, startSeq: number): void {
    for (const [taskId, op] of this.#mutationOverlay) {
      if (op.seq <= startSeq) continue
      if (op.kind === 'remove') entries.delete(taskId)
      else entries.set(taskId, Object.freeze({ snapshot: op.snapshot, updatedAt: op.updatedAt }))
    }
    this.#fullEntries = entries
    this.#fullKind = entries.size === 0 ? 'empty' : 'available'
    this.#rebuildFullTasksCache()
    this.#syncShareIndexFromFull()
  }

  #syncShareIndexFromFull(): void {
    if (this.#shareIndex.kind !== 'ready' && this.#shareIndex.kind !== 'failed') return
    const shareInstance = this.#shareIndex.shareInstance
    const entries = new Map<string, ShareIndexEntry>()
    for (const [taskId, entry] of this.#fullEntries) {
      if (entry.snapshot.shareInstance !== shareInstance) continue
      entries.set(taskId, Object.freeze({
        kind: 'resolved',
        snapshot: entry.snapshot,
        updatedAt: entry.updatedAt,
      }))
    }
    this.#shareIndex = Object.freeze({ kind: 'ready', shareInstance, entries })
  }

  #applyOverlayToShareEntries(
    entries: Map<string, ShareIndexEntry>,
    shareInstance: string,
    startSeq: number,
  ): void {
    for (const [taskId, op] of this.#mutationOverlay) {
      if (op.seq <= startSeq) continue
      if (op.kind === 'remove') {
        entries.delete(taskId)
      } else if (op.snapshot.shareInstance === shareInstance) {
        entries.set(taskId, Object.freeze({
          kind: 'resolved',
          snapshot: op.snapshot,
          updatedAt: op.updatedAt,
        }))
      }
    }
  }

  #collectShareEntries(shareInstance: string, key: string): ShareIndexEntry[] {
    const matches: ShareIndexEntry[] = []
    if (
      (this.#shareIndex.kind === 'ready' || this.#shareIndex.kind === 'failed') &&
      this.#shareIndex.shareInstance === shareInstance
    ) {
      for (const entry of this.#shareIndex.entries.values()) {
        if (entryMatchesKey(entry, shareInstance, key)) matches.push(entry)
      }
      return matches
    }
    if (this.#fullKind === 'available') {
      for (const entry of this.#fullEntries.values()) {
        if (entry.snapshot.shareInstance === shareInstance && entry.snapshot.selectionKey === key) {
          matches.push(Object.freeze({ kind: 'resolved', snapshot: entry.snapshot, updatedAt: entry.updatedAt }))
        }
      }
    }
    return matches
  }

  #upsertIntoShareIndex(shareInstance: string, entry: Extract<ShareIndexEntry, { kind: 'resolved' }>): void {
    if (
      (this.#shareIndex.kind === 'ready' || this.#shareIndex.kind === 'failed') &&
      this.#shareIndex.shareInstance === shareInstance
    ) {
      this.#shareIndex.entries.set(entry.snapshot.taskId, Object.freeze(entry))
    }
  }

  #writeResolvedSnapshot(snapshot: RetainedTaskSnapshot, updatedAt: number): void {
    const entry: ResolvedInventoryEntry = Object.freeze({ snapshot, updatedAt })
    if (this.#fullKind === 'available' || this.#fullKind === 'empty') {
      this.#fullEntries.set(snapshot.taskId, entry)
      this.#fullKind = 'available'
      this.#rebuildFullTasksCache()
    }
    this.#upsertIntoShareIndex(snapshot.shareInstance, { kind: 'resolved', snapshot, updatedAt })
  }

  #pruneMissingCandidate(taskId: string): void {
    emitOutputTrace(this.#outputTrace, 'continuation', {
      task_id: taskId,
      transition: 'candidate_pruned',
    })
    this.removeTask(taskId)
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

function entryTaskId(entry: ShareIndexEntry): string {
  return entry.kind === 'raw' ? entry.record.taskId : entry.snapshot.taskId
}

function entryMatchesKey(entry: ShareIndexEntry, shareInstance: string, key: string): boolean {
  if (entry.kind === 'raw') {
    return entry.record.shareInstance === shareInstance &&
      entry.record.selectionKey === key &&
      isSingleFileReuseCandidate(entry.record.backend, entry.record.lifecycle)
  }
  return entry.snapshot.shareInstance === shareInstance &&
    entry.snapshot.selectionKey === key &&
    isSingleFileReuseCandidate(entry.snapshot.backend, entry.snapshot.lifecycle)
}

function entryNeedsResolution(entry: ShareIndexEntry): boolean {
  if (entry.kind === 'raw') return true
  const { lifecycle, activeElsewhere } = entry.snapshot
  return activeElsewhere || lifecycle.kind === 'downloading' || lifecycle.kind === 'preparing'
}
