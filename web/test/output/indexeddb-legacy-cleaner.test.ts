import { describe, expect, it } from 'vitest'

import {
  cleanIndexedDbLegacyStores,
  type IndexedDbLegacyCleanupDatabase,
  type IndexedDbLegacyCleanupMetadata,
  type IndexedDbLegacyCleanupPorts,
  type IndexedDbLegacyCleanupTransition,
} from '../../src/output/browser/indexeddb-legacy-cleaner'

const LEGACY_STORE_NAMES = [
  'checkpoint-candidates',
  'checkpoint-committed',
  'persistent-handles',
  'cleanup-markers',
] as const

interface MemoryCleanupState {
  metadata?: unknown
  readonly legacyRecords: Map<string, unknown[]>
  readonly currentRecords: Map<string, unknown>
  readonly clearedStores: string[]
  readonly committedSteps: number[]
  crashAfterStep: number | undefined
  pauseAfterStep: number | undefined
  pauseStarted?: Deferred<void>
  pauseRelease?: Deferred<void>
  openCount: number
  closeCount: number
}

describe('IndexedDB legacy cleaner', () => {
  it('clears opaque records only from the exact application-owned legacy stores', async () => {
    const state = memoryState(2)
    const currentBefore = [...state.currentRecords.entries()]
    const ports = memoryPorts(state)

    await expect(cleanIndexedDbLegacyStores(ports)).resolves.toEqual({
      status: 'completed',
      removed: LEGACY_STORE_NAMES.length * 2,
    })
    expect(state.clearedStores).toEqual(LEGACY_STORE_NAMES)
    expect([...state.legacyRecords.values()].every((records) => records.length === 0)).toBe(true)
    expect([...state.currentRecords.entries()]).toEqual(currentBefore)
    await expect(cleanIndexedDbLegacyStores(ports)).resolves.toEqual({
      status: 'nothing-to-clean',
      removed: 0,
    })
    expect(state.clearedStores).toEqual(LEGACY_STORE_NAMES)
  })

  it.each([0, 1, 2, 3, 4])(
    'resumes after durable step %i without replaying a committed clear',
    async (crashAfterStep) => {
      const state = memoryState(1)
      const ports = memoryPorts(state)
      state.crashAfterStep = crashAfterStep

      await expect(cleanIndexedDbLegacyStores(ports)).rejects.toThrow(
        `simulated interruption after step ${crashAfterStep}`,
      )
      expect(metadataStep(state.metadata)).toBe(crashAfterStep)

      await expect(cleanIndexedDbLegacyStores(ports)).resolves.toEqual({
        status: crashAfterStep === LEGACY_STORE_NAMES.length
          ? 'nothing-to-clean'
          : 'completed',
        removed: LEGACY_STORE_NAMES.length - crashAfterStep,
      })
      expect(state.clearedStores).toEqual(LEGACY_STORE_NAMES)
      expect(state.committedSteps).toEqual([0, 1, 2, 3, 4])
      await expect(cleanIndexedDbLegacyStores(ports)).resolves.toEqual({
        status: 'nothing-to-clean',
        removed: 0,
      })
      expect(state.clearedStores).toEqual(LEGACY_STORE_NAMES)
      expect(state.openCount).toBe(3)
      expect(state.closeCount).toBe(3)
    },
  )

  it('holds the cleanup lease until a committed pass returns', async () => {
    const state = memoryState(1)
    const lease = new SerialLease()
    const ports = memoryPorts(state, lease)
    state.pauseAfterStep = 1
    state.pauseStarted = deferred<void>()
    state.pauseRelease = deferred<void>()

    const first = cleanIndexedDbLegacyStores(ports)
    await state.pauseStarted.promise
    const second = cleanIndexedDbLegacyStores(ports)
    await Promise.resolve()
    expect(state.openCount).toBe(1)
    expect(lease.maximumActive).toBe(1)

    state.pauseRelease.resolve()
    await expect(Promise.all([first, second])).resolves.toEqual([
      { status: 'completed', removed: LEGACY_STORE_NAMES.length },
      { status: 'nothing-to-clean', removed: 0 },
    ])
    expect(state.openCount).toBe(2)
    expect(lease.maximumActive).toBe(1)
    expect(lease.active).toBe(0)
  })

  it('fails closed when the persistent marker is not the cleaner schema', async () => {
    const state = memoryState(1)
    state.metadata = Object.freeze({ step: 0, state: 'pending', removed: 0 })

    await expect(cleanIndexedDbLegacyStores(memoryPorts(state))).rejects.toThrow(
      'unknown ownership or progress',
    )
    expect(state.clearedStores).toEqual([])
    expect([...state.legacyRecords.values()].every((records) => records.length === 1)).toBe(true)
  })
})

class MemoryCleanupDatabase implements IndexedDbLegacyCleanupDatabase {
  readonly #state: MemoryCleanupState

  constructor(state: MemoryCleanupState) {
    this.#state = state
  }

  async readOrInitializeMetadata(initial: IndexedDbLegacyCleanupMetadata): Promise<unknown> {
    if (this.#state.metadata === undefined) {
      this.#state.metadata = cloneMetadata(initial)
      this.#state.committedSteps.push(0)
      this.#interruptAfter(0)
    }
    return cloneUnknown(this.#state.metadata)
  }

  async clearLegacyStore(
    storeName: string,
    expected: IndexedDbLegacyCleanupMetadata,
    transition: IndexedDbLegacyCleanupTransition,
  ): Promise<unknown> {
    if (!sameMetadata(this.#state.metadata, expected)) {
      throw new Error('memory cleanup metadata changed outside its lease')
    }
    const records = this.#state.legacyRecords.get(storeName) ?? []
    const next: IndexedDbLegacyCleanupMetadata = Object.freeze({
      ...expected,
      ...transition,
      removed: expected.removed + records.length,
    })
    this.#state.legacyRecords.set(storeName, [])
    this.#state.metadata = next
    this.#state.clearedStores.push(storeName)
    this.#state.committedSteps.push(transition.step)
    if (this.#state.pauseAfterStep === transition.step) {
      this.#state.pauseAfterStep = undefined
      this.#state.pauseStarted?.resolve()
      await this.#state.pauseRelease?.promise
    }
    this.#interruptAfter(transition.step)
    return cloneMetadata(next)
  }

  close(): void {
    this.#state.closeCount += 1
  }

  #interruptAfter(step: number): void {
    if (this.#state.crashAfterStep !== step) return
    this.#state.crashAfterStep = undefined
    throw new Error(`simulated interruption after step ${step}`)
  }
}

class SerialLease {
  #tail: Promise<void> = Promise.resolve()
  active = 0
  maximumActive = 0

  async acquire(): Promise<{ readonly release: () => Promise<void> }> {
    const predecessor = this.#tail
    const released = deferred<void>()
    this.#tail = predecessor.then(() => released.promise)
    await predecessor
    this.active += 1
    this.maximumActive = Math.max(this.maximumActive, this.active)
    let settled = false
    return Object.freeze({
      release: async () => {
        if (settled) return
        settled = true
        this.active -= 1
        released.resolve()
      },
    })
  }
}

interface Deferred<T> {
  readonly promise: Promise<T>
  readonly resolve: (value: T | PromiseLike<T>) => void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>((complete) => { resolve = complete })
  return { promise, resolve }
}

function memoryState(recordsPerStore: number): MemoryCleanupState {
  return {
    legacyRecords: new Map(LEGACY_STORE_NAMES.map((storeName) => [
      storeName,
      Array.from({ length: recordsPerStore }, (_, index) => Object.freeze({
        id: `${storeName}-${index}`,
        opaquePayload: new Uint8Array([index]),
      })),
    ])),
    currentRecords: new Map([
      ['current-file-checkpoint-v1', Object.freeze({ published: true })],
    ]),
    clearedStores: [],
    committedSteps: [],
    crashAfterStep: undefined,
    pauseAfterStep: undefined,
    openCount: 0,
    closeCount: 0,
  }
}

function memoryPorts(
  state: MemoryCleanupState,
  lease = new SerialLease(),
): IndexedDbLegacyCleanupPorts {
  return {
    acquireLease: () => lease.acquire(),
    openDatabase: async () => {
      state.openCount += 1
      return new MemoryCleanupDatabase(state)
    },
  }
}

function metadataStep(value: unknown): number | undefined {
  return typeof value === 'object' && value !== null && 'step' in value
    ? (value as { readonly step?: number }).step
    : undefined
}

function cloneMetadata(
  value: IndexedDbLegacyCleanupMetadata,
): IndexedDbLegacyCleanupMetadata {
  return { ...value }
}

function cloneUnknown(value: unknown): unknown {
  return typeof value === 'object' && value !== null ? { ...value } : value
}

function sameMetadata(left: unknown, right: IndexedDbLegacyCleanupMetadata): boolean {
  if (typeof left !== 'object' || left === null) return false
  return JSON.stringify(left) === JSON.stringify(right)
}
