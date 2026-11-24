import type { BrowserOutputWindow } from '../../output/backends/contract'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import type { OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveTaskRecord } from '../../output/store/types'
import type { PendingRetainedAction, RetainedInventorySnapshot, RetainedTaskSnapshot } from './contracts'
import type { ReceiveStorage, ReceiveStoreAccess } from './receive-storage'
import { resolveRetainedCandidates, retainedSnapshot, retainedTaskEnvironment } from './retained'

type ResolvedInventoryEntry = Readonly<{
  snapshot: RetainedTaskSnapshot
  updatedAt: number
}>

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

  #fullKind: RetainedInventorySnapshot['kind'] = 'unloaded'
  #fullEntries = new Map<string, ResolvedInventoryEntry>()
  #fullTasksCache: readonly RetainedTaskSnapshot[] = Object.freeze([])
  #fullLoadEpoch = 0

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

  #commitFullEntries(entries: Map<string, ResolvedInventoryEntry>, startSeq: number): void {
    for (const [taskId, op] of this.#mutationOverlay) {
      if (op.seq <= startSeq) continue
      if (op.kind === 'remove') entries.delete(taskId)
      else entries.set(taskId, Object.freeze({ snapshot: op.snapshot, updatedAt: op.updatedAt }))
    }
    this.#fullEntries = entries
    this.#fullKind = entries.size === 0 ? 'empty' : 'available'
    this.#rebuildFullTasksCache()
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
