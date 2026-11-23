import type { ReceiveTaskRecord } from '../../output/store/types'
import type { ReceiveStorage } from './receive-storage'

type ShareTaskIndexState =
  | Readonly<{ kind: 'idle'; shareInstance: null }>
  | Readonly<{ kind: 'loading'; shareInstance: string; promise: Promise<readonly ReceiveTaskRecord[]> }>
  | Readonly<{ kind: 'ready'; shareInstance: string; records: readonly ReceiveTaskRecord[] }>
  | Readonly<{ kind: 'failed'; shareInstance: string }>

const IDLE_INDEX: ShareTaskIndexState = Object.freeze({ kind: 'idle', shareInstance: null })

/**
 * Caches a single `listTasksByShare(shareInstance)` lookup per joined share so
 * single-file quick downloads never scan unrelated shares or normalize non-matching tasks.
 */
export class ShareTaskIndex {
  #state: ShareTaskIndexState = IDLE_INDEX

  preload(
    shareInstance: string,
    hasIndexedDb: boolean,
    storage: ReceiveStorage,
    isDisposed: () => boolean,
    onChange: () => void,
  ): void {
    if (isDisposed()) return
    if (
      this.#state.shareInstance === shareInstance &&
      (this.#state.kind === 'loading' || this.#state.kind === 'ready')
    ) {
      return
    }
    if (!hasIndexedDb) {
      this.#state = Object.freeze({ kind: 'ready', shareInstance, records: Object.freeze([]) })
      return
    }
    let resolveRecords!: (records: readonly ReceiveTaskRecord[]) => void
    const promise = new Promise<readonly ReceiveTaskRecord[]>(resolve => {
      resolveRecords = resolve
    })
    const loading: ShareTaskIndexState = Object.freeze({ kind: 'loading', shareInstance, promise })
    this.#state = loading
    onChange()

    void storage.run(async access => {
      try {
        const store = await access.openStoreOptional()
        if (isDisposed() || this.#state !== loading) {
          resolveRecords(Object.freeze([]))
          return
        }
        const records = store === undefined
          ? Object.freeze([])
          : await store.listTasksByShare(shareInstance)
        if (isDisposed() || this.#state !== loading) {
          resolveRecords(Object.freeze([]))
          return
        }
        this.#state = Object.freeze({ kind: 'ready', shareInstance, records })
        resolveRecords(records)
        onChange()
      } catch {
        if (isDisposed() || this.#state !== loading) {
          resolveRecords(Object.freeze([]))
          return
        }
        this.#state = Object.freeze({ kind: 'failed', shareInstance })
        resolveRecords(Object.freeze([]))
        onChange()
      }
    })
  }

  isLoadingFor(shareInstance: string): boolean {
    return this.#state.kind === 'loading' && this.#state.shareInstance === shareInstance
  }

  pendingPromiseFor(shareInstance: string): Promise<readonly ReceiveTaskRecord[]> | undefined {
    return this.#state.kind === 'loading' && this.#state.shareInstance === shareInstance
      ? this.#state.promise
      : undefined
  }

  recordsFor(shareInstance: string, key: string): readonly ReceiveTaskRecord[] {
    if (this.#state.kind !== 'ready' || this.#state.shareInstance !== shareInstance) {
      return Object.freeze([])
    }
    return this.#state.records.filter(record => record.selectionKey === key)
  }

  upsert(record: ReceiveTaskRecord): void {
    if (this.#state.kind !== 'ready' || this.#state.shareInstance !== record.shareInstance) return
    const next = [record, ...this.#state.records.filter(existing => existing.taskId !== record.taskId)]
    next.sort((left, right) => right.updatedAt - left.updatedAt)
    this.#state = Object.freeze({
      kind: 'ready',
      shareInstance: this.#state.shareInstance,
      records: Object.freeze(next),
    })
  }

  remove(taskId: string): void {
    if (this.#state.kind !== 'ready') return
    const filtered = this.#state.records.filter(record => record.taskId !== taskId)
    if (filtered.length === this.#state.records.length) return
    this.#state = Object.freeze({
      kind: 'ready',
      shareInstance: this.#state.shareInstance,
      records: Object.freeze(filtered),
    })
  }

  /** Reconciles the cached per-share list with a full IDB scan so externally deleted records disappear. */
  syncFromAllRecords(records: readonly ReceiveTaskRecord[]): void {
    if (this.#state.kind !== 'ready' && this.#state.kind !== 'failed') return
    const shareInstance = this.#state.shareInstance
    const matching = records.filter(record => record.shareInstance === shareInstance)
    matching.sort((left, right) => right.updatedAt - left.updatedAt)
    this.#state = Object.freeze({
      kind: 'ready',
      shareInstance,
      records: Object.freeze(matching),
    })
  }

  reset(): void {
    this.#state = IDLE_INDEX
  }
}
