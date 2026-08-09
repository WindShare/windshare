import { describe, expect, it } from 'vitest'

import {
  INDEXEDDB_LEGACY_V5_STORES,
} from '../../src/output/browser/indexeddb-database'
import {
  INDEXEDDB_LEGACY_CLEANUP_ORDER,
  cleanIndexedDbLegacyStores,
  type IndexedDbLegacyCleanupDatabase,
  type IndexedDbLegacyCleanupPage,
  type IndexedDbLegacyCleanupPorts,
  type IndexedDbLegacyCleanupProgress,
  type IndexedDbLegacyRow,
  type IndexedDbLegacyStoreName,
  type LegacyOwnershipDecision,
} from '../../src/output/browser/indexeddb-legacy-cleaner'

describe('ownership-proven IndexedDB v5 cleanup', () => {
  it('owns the exact v5 store set and no resume-compatible legacy source', () => {
    expect(new Set(INDEXEDDB_LEGACY_CLEANUP_ORDER)).toEqual(
      new Set(INDEXEDDB_LEGACY_V5_STORES),
    )
  })

  it('deletes each certified record without clearing a whole legacy store', async () => {
    const state = memoryState(2)
    await expect(cleanIndexedDbLegacyStores(memoryPorts(state))).resolves.toEqual({
      status: 'completed',
      removed: INDEXEDDB_LEGACY_CLEANUP_ORDER.length * 2,
    })
    expect([...state.rows.values()].every((rows) => rows.size === 0)).toBe(true)
    expect(state.deleted).toHaveLength(INDEXEDDB_LEGACY_CLEANUP_ORDER.length * 2)

    await expect(cleanIndexedDbLegacyStores(memoryPorts(state))).resolves.toEqual({
      status: 'nothing-to-clean',
      removed: 0,
    })
  })

  it('fails closed at an ownership-unknown row and never offers it for resume', async () => {
    const state = memoryState(1)
    const storeName = INDEXEDDB_LEGACY_CLEANUP_ORDER[2]
    const row = state.rows.get(storeName)!.values().next().value as MemoryRow
    row.decision = 'unknown'

    await expect(cleanIndexedDbLegacyStores(memoryPorts(state))).resolves.toEqual({
      status: 'needs-attention',
      removed: 2,
      storeName,
      key: row.key,
      decision: 'unknown',
    })
    expect(state.rows.get(storeName)!.has(row.key)).toBe(true)
    expect(state.deleted).not.toContain(`${storeName}:${row.key}`)
  })

  it.each([0, 1, 4, 8, 12, 16])(
    'resumes exactly after committed crash cut %i',
    async (crashAfterCommit) => {
      const state = memoryState(1)
      state.crashAfterCommit = crashAfterCommit

      await expect(cleanIndexedDbLegacyStores(memoryPorts(state)))
        .rejects.toThrow(`crash-after-commit:${crashAfterCommit}`)
      const removedAtCut = progress(state).removed

      await expect(cleanIndexedDbLegacyStores(memoryPorts(state))).resolves.toEqual({
        status: removedAtCut === INDEXEDDB_LEGACY_CLEANUP_ORDER.length
          ? 'nothing-to-clean'
          : 'completed',
        removed: INDEXEDDB_LEGACY_CLEANUP_ORDER.length - removedAtCut,
      })
      expect(new Set(state.deleted).size).toBe(INDEXEDDB_LEGACY_CLEANUP_ORDER.length)
      expect([...state.rows.values()].every((rows) => rows.size === 0)).toBe(true)
    },
  )
})

interface MemoryRow extends IndexedDbLegacyRow {
  decision: LegacyOwnershipDecision
}

interface MemoryState {
  progress?: IndexedDbLegacyCleanupProgress
  readonly rows: Map<IndexedDbLegacyStoreName, Map<string, MemoryRow>>
  readonly deleted: string[]
  commits: number
  crashAfterCommit?: number
}

class MemoryDatabase implements IndexedDbLegacyCleanupDatabase {
  readonly #state: MemoryState

  constructor(state: MemoryState) {
    this.#state = state
  }

  async readOrInitializeProgress(initial: IndexedDbLegacyCleanupProgress): Promise<unknown> {
    if (this.#state.progress === undefined) {
      this.#state.progress = { ...initial }
      this.#commit()
    }
    return { ...this.#state.progress }
  }

  async scan(
    storeName: IndexedDbLegacyStoreName,
    afterKey: string | undefined,
    limit: number,
  ): Promise<IndexedDbLegacyCleanupPage> {
    const rows = [...this.#state.rows.get(storeName)!.values()]
      .filter((row) => afterKey === undefined || row.key > afterKey)
      .sort((left, right) => left.key.localeCompare(right.key))
    return {
      rows: rows.slice(0, limit),
      done: rows.length <= limit,
    }
  }

  async certifyAndDelete(
    storeName: IndexedDbLegacyStoreName,
    row: IndexedDbLegacyRow,
    expected: IndexedDbLegacyCleanupProgress,
    next: IndexedDbLegacyCleanupProgress,
  ): Promise<LegacyOwnershipDecision> {
    this.#assertProgress(expected)
    const current = this.#state.rows.get(storeName)!.get(row.key)
    if (current === undefined) throw new Error('missing memory legacy row')
    if (current.decision !== 'owned') return current.decision
    this.#state.rows.get(storeName)!.delete(row.key)
    this.#state.deleted.push(`${storeName}:${row.key}`)
    this.#state.progress = { ...next }
    this.#commit()
    return 'owned'
  }

  async advanceStore(
    expected: IndexedDbLegacyCleanupProgress,
    next: IndexedDbLegacyCleanupProgress,
  ): Promise<void> {
    this.#assertProgress(expected)
    this.#state.progress = { ...next }
    this.#commit()
  }

  close(): void {}

  #assertProgress(expected: IndexedDbLegacyCleanupProgress): void {
    const current = this.#state.progress
    if (current === undefined ||
        current.id !== expected.id ||
        current.operationId !== expected.operationId ||
        current.kind !== expected.kind ||
        current.schemaVersion !== expected.schemaVersion ||
        current.storeIndex !== expected.storeIndex ||
        current.afterKey !== expected.afterKey ||
        current.removed !== expected.removed ||
        current.state !== expected.state) {
      throw new Error('memory cleanup progress changed')
    }
  }

  #commit(): void {
    const committed = this.#state.commits
    this.#state.commits += 1
    if (this.#state.crashAfterCommit !== committed) return
    delete this.#state.crashAfterCommit
    throw new Error(`crash-after-commit:${committed}`)
  }
}

function memoryState(rowsPerStore: number): MemoryState {
  return {
    rows: new Map(INDEXEDDB_LEGACY_CLEANUP_ORDER.map((storeName) => [
      storeName,
      new Map(Array.from({ length: rowsPerStore }, (_, index) => {
        const key = `${storeName}-${index}`
        return [key, { key, value: { opaque: index }, decision: 'owned' as const }]
      })),
    ])),
    deleted: [],
    commits: 0,
  }
}

function memoryPorts(state: MemoryState): IndexedDbLegacyCleanupPorts {
  return {
    acquireLease: async () => ({ release: async () => undefined }),
    openDatabase: async () => new MemoryDatabase(state),
  }
}

function progress(state: MemoryState): IndexedDbLegacyCleanupProgress {
  if (state.progress === undefined) throw new Error('cleanup progress was not initialized')
  return state.progress
}
