import { describe, expect, it, vi } from 'vitest'
import { withStagingExportAuthority, type StagingExportAuthority } from '../../src/output/staging-budget/export-authority'
import { StagingBudgetCoordinator, type StagingFileReservation } from '../../src/output/staging-budget/coordinator'
import type { StagingBudgetInventory, StagingBudgetMutation, StagingBudgetPolicy, StagingBudgetStore,
  StagingBudgetRecord } from '../../src/output/staging-budget/contracts'
import type { BrowserStagingStorageFacts } from '../../src/output/planning/staging-storage'

const policy: StagingBudgetPolicy = { maximumTaskFiles: 2, maximumSiteFiles: 3,
  maximumTaskPhysicalBytes: 1000n, maximumSitePhysicalBytes: 2000n,
  metadataHeadroomBytes: 10n, finalizationHeadroomBytes: 20n, minimumQuotaReserveBytes: 100n }
const normal: BrowserStagingStorageFacts = { opfs: 'usable', persistence: 'not-persisted',
  quota: { kind: 'estimated', usageBytes: 0n, quotaBytes: 2000n }, pressure: 'normal' }

class MemoryStore implements StagingBudgetStore {
  readonly coordinationScope = 'context' as const
  records: StagingBudgetRecord[] = []
  workspace = { occupiedBytes: 0n, outstandingBytes: 0n }
  transact<T>(update: (inventory: StagingBudgetInventory) => StagingBudgetMutation<T>): Promise<T> {
    try {
      const mutation = update({ records: this.records, workspace: this.workspace })
      if (mutation.put !== undefined) this.records = [...this.records.filter(r => r.id !== mutation.put!.id), mutation.put]
      if (mutation.deleteId !== undefined) this.records = this.records.filter(r => r.id !== mutation.deleteId)
      for (const record of mutation.puts ?? []) this.records = [...this.records.filter(r => r.id !== record.id), record]
      for (const id of mutation.deleteIds ?? []) this.records = this.records.filter(r => r.id !== id)
      return Promise.resolve(mutation.result)
    } catch (error) { return Promise.reject(error) }
  }
}

function coordinator(store = new MemoryStore(), storage = normal, overrides: Partial<StagingBudgetPolicy> = {}) {
  return new StagingBudgetCoordinator({ store, storage: async () => storage, policy: { ...policy, ...overrides } })
}
async function reserve(subject: StagingBudgetCoordinator, fileId = 'file', operationId = 'operation', exactSize = 100n) {
  const result = await subject.tryReserve({ operationId, fileId, exactSize })
  if (result.kind !== 'admitted') throw new Error(result.reason)
  return result.reservation
}
async function queue(file: StagingFileReservation, bytes = 100n) { await file.received(bytes); await file.queueExport() }

describe('full-file staging and one-export budget', () => {
  it('reserves a complete file, headroom, and one target copy without treating quota as destination free space', async () => {
    const subject = coordinator(new MemoryStore(), { ...normal, quota: { kind: 'estimated', usageBytes: 0n, quotaBytes: 230n } })
    await reserve(subject)
    expect(await subject.snapshot()).toEqual({ verifiedStagedBytes: 0n, outstandingBytes: 130n,
      reservedStagingBytes: 130n, oneExportBytes: 100n, physicalDemandBytes: 230n })
    expect(await subject.tryReserve({ operationId: 'other', fileId: 'next', exactSize: 1n }))
      .toEqual({ kind: 'deferred', reason: 'quota-insufficient' })
  })

  it('bounds task/site backlog and preserves all staged bytes while export fails and retries', () => withStagingExportAuthority(async authority => {
    const subject = coordinator()
    const first = await reserve(subject, 'first')
    const second = await reserve(subject, 'second')
    expect(await subject.tryReserve({ operationId: 'operation', fileId: 'third', exactSize: 1n }))
      .toMatchObject({ reason: 'task-file-limit' })
    await queue(first)
    expect(await subject.tryReserve({ operationId: 'other', fileId: 'next', exactSize: 1n }))
      .toMatchObject({ reason: 'drain-first' })
    await queue(second)
    expect(await first.beginExport(authority)).toBe(true)
    expect(await second.beginExport(authority)).toBe(false)
    await first.exportFailed()
    expect((await subject.snapshot()).reservedStagingBytes).toBe(260n)
    expect(await second.beginExport(authority)).toBe(true)
    await second.targetSaved()
    expect((await subject.snapshot()).reservedStagingBytes).toBe(260n)
    await second.releaseDeleted()
    expect((await subject.snapshot()).reservedStagingBytes).toBe(130n)
    expect(await first.beginExport(authority)).toBe(true)
    await first.targetSaved()
    await first.releaseDeleted()
    expect((await subject.snapshot()).physicalDemandBytes).toBe(0n)
  }))

  it('shares limits and other workspace reservations across simultaneous tasks', async () => {
    const store = new MemoryStore()
    const first = coordinator(store)
    const other = coordinator(store)
    await reserve(first, 'one', 'first')
    await reserve(first, 'two', 'first')
    await reserve(other, 'one', 'other')
    expect(await other.tryReserve({ operationId: 'third', fileId: 'one', exactSize: 1n }))
      .toMatchObject({ reason: 'site-file-limit' })
    store.workspace.outstandingBytes = 1800n
    const blocked = coordinator(store, normal, { maximumSiteFiles: 4 })
    expect(await blocked.tryReserve({ operationId: 'fourth', fileId: 'one', exactSize: 1n }))
      .toMatchObject({ reason: 'quota-insufficient' })
  })

  it('uses a separate physical-demand cap and can admit with unknown browser quota', async () => {
    const unknown = { ...normal, quota: { kind: 'unknown' as const } }
    const subject = coordinator(new MemoryStore(), unknown)
    expect(await subject.tryReserve({ operationId: 'task', fileId: 'big', exactSize: 500n }))
      .toMatchObject({ reason: 'task-physical-limit' })
    const site = coordinator(new MemoryStore(), unknown, { maximumSitePhysicalBytes: 200n })
    expect(await site.tryReserve({ operationId: 'task', fileId: 'big', exactSize: 100n }))
      .toMatchObject({ reason: 'site-physical-limit' })
    await reserve(subject)
  })

  it('reconstructs retained obligations under pressure and fences old handles without dropping recovery data', () => withStagingExportAuthority(async authority => {
    const store = new MemoryStore()
    const subject = coordinator(store)
    const old = await reserve(subject)
    await old.received(50n)
    const reopened = coordinator(store, { ...normal, pressure: 'drain-first', quota: { kind: 'estimated', usageBytes: 50n, quotaBytes: 0n } })
    const current = await reopened.restore({ operationId: 'operation', fileId: 'file', exactSize: 100n,
      verifiedStagedBytes: 50n, phase: 'receiving', operationLease: { operationId: 'operation', leaseId: 'fresh' } })
    await expect(old.received(60n)).rejects.toThrow('ownership changed')
    expect((await reopened.snapshot()).outstandingBytes).toBe(80n)
    await current.received(100n)
    await current.queueExport()
    expect(await current.beginExport(authority)).toBe(true)
    await current.targetSaved()
    await current.releaseDeleted()
  }))

  it('reconciles crash-cut unused claims only under their operation journal authority', async () => {
    const store = new MemoryStore()
    const subject = coordinator(store, normal, { maximumTaskFiles: 10, maximumSiteFiles: 10 })
    const abandoned = await reserve(subject, 'orphan')
    await reserve(subject, 'journaled')
    const used = await reserve(subject, 'used')
    await used.objectCapacity.reserveGrowth({ operationId: 'operation', objectId: 'object',
      currentLength: 0n, targetLength: 100n, metadataHeadroom: 0n })
    const durable = await reserve(subject, 'durable')
    await durable.received(50n)
    await reserve(subject, 'another-task', 'other')
    const before = await subject.snapshot()
    expect(await subject.reconcileUnused({ operationId: 'operation', deliveryFileIds: ['journaled'],
      operationLease: { operationId: 'operation', leaseId: 'reopened' } })).toBe(1)
    expect((await subject.snapshot()).reservedStagingBytes).toBe(before.reservedStagingBytes - 130n)
    expect(store.records.map(record => record.fileId).sort()).toEqual(['another-task', 'durable', 'journaled', 'used'])
    await expect(abandoned.received(1n)).rejects.toThrow('ownership changed')
    expect(await subject.reconcileUnused({ operationId: 'operation', deliveryFileIds: ['journaled'],
      operationLease: { operationId: 'operation', leaseId: 'reopened' } })).toBe(0)
    await expect(subject.reconcileUnused({ operationId: 'operation', deliveryFileIds: [],
      operationLease: { operationId: 'other', leaseId: 'wrong' } })).rejects.toThrow('operation lease')
    await reserve(subject, 'orphan')
  })

  it('recovers a vanished exporter under a fresh exclusive authority while preserving its file', async () => {
    const store = new MemoryStore()
    const subject = coordinator(store)
    const lost = await reserve(subject, 'lost', 'lost-task')
    const other = await reserve(subject, 'other', 'other-task')
    await queue(lost)
    await queue(other)
    let ended: StagingExportAuthority | undefined
    await withStagingExportAuthority(async authority => {
      ended = authority
      expect(await lost.beginExport(authority)).toBe(true)
    })
    const before = await subject.snapshot()
    await expect(other.beginExport(ended!)).rejects.toThrow('current exclusive')
    await expect(lost.targetSaved()).rejects.toThrow('current exclusive')
    await withStagingExportAuthority(async authority => {
      expect(await other.beginExport(authority)).toBe(true)
      expect(store.records.find(record => record.fileId === 'lost')?.phase).toBe('export-failed')
      expect(await subject.snapshot()).toEqual(before)
      await other.targetSaved()
      await other.releaseDeleted()
    })
    expect((await subject.snapshot()).reservedStagingBytes).toBe(130n)
    await reserve(subject, 'new-work', 'new-task')
    expect((await subject.snapshot()).reservedStagingBytes).toBe(260n)
  })

  it('fences a destination-state mutation queued beyond the exporter callback lifetime', async () => {
    const memory = new MemoryStore()
    let release: (() => void) | undefined
    let delay: Promise<void> | undefined
    const store: StagingBudgetStore = { coordinationScope: 'context',
      transact: async update => { await delay; return memory.transact(update) } }
    const subject = new StagingBudgetCoordinator({ store, storage: async () => normal, policy })
    const file = await reserve(subject)
    await queue(file)
    let completion: Promise<void> | undefined
    await withStagingExportAuthority(async authority => {
      expect(await file.beginExport(authority)).toBe(true)
      delay = new Promise<void>(resolve => { release = resolve })
      completion = file.targetSaved()
      completion.catch(() => undefined)
    })
    release!()
    await expect(completion).rejects.toThrow('current exclusive')
    expect(memory.records[0]?.phase).toBe('exporting')
  })

  it('rejects context-only authority for an origin-shared budget', async () => {
    const memory = new MemoryStore()
    const origin: StagingBudgetStore = { coordinationScope: 'origin',
      transact: update => memory.transact(update) }
    const subject = new StagingBudgetCoordinator({ store: origin, storage: async () => normal, policy })
    const file = await reserve(subject)
    await queue(file)
    vi.stubGlobal('navigator', {})
    try {
      await withStagingExportAuthority(async authority => {
        await expect(file.beginExport(authority)).rejects.toThrow('current exclusive')
      })
      expect(memory.records[0]?.phase).toBe('queued')
    } finally { vi.unstubAllGlobals() }
  })

  it('keeps used failed reservations until explicit ownership-confirmed discard', async () => {
    const subject = coordinator()
    const file = await reserve(subject)
    await file.objectCapacity.reserveGrowth({ operationId: 'operation', objectId: 'object',
      currentLength: 0n, targetLength: 50n, metadataHeadroom: 0n })
    await expect(file.cancelUnused()).rejects.toThrow('used staging')
    await file.releaseDiscarded()
    expect((await subject.snapshot()).reservedStagingBytes).toBe(0n)
    await expect(file.received(1n)).rejects.toThrow('ownership changed')
  })

  it('rejects early deletion, incomplete export, unreserved growth, and retained cancellation', async () => {
    const subject = coordinator()
    const file = await reserve(subject)
    await expect(file.queueExport()).rejects.toThrow('complete verified')
    await expect(file.releaseDeleted()).rejects.toThrow('phase changed')
    await expect(file.objectCapacity.reserveGrowth({ operationId: 'operation', objectId: 'object',
      currentLength: 0n, targetLength: 101n, metadataHeadroom: 0n })).rejects.toThrow('escaped')
    const growth = await file.objectCapacity.reserveGrowth({ operationId: 'operation', objectId: 'object',
      currentLength: 0n, targetLength: 100n, metadataHeadroom: 1n })
    await growth.release()
    await expect(file.objectCapacity.reserveGrowth({ operationId: 'operation', objectId: 'other-object',
      currentLength: 0n, targetLength: 100n, metadataHeadroom: 0n })).rejects.toThrow('another object')
    expect((await subject.snapshot()).reservedStagingBytes).toBe(130n)
    await file.received(50n)
    await expect(file.cancelUnused()).rejects.toThrow('used staging')
    const unused = await reserve(subject, 'unused')
    await unused.cancelUnused()
    expect((await subject.snapshot()).reservedStagingBytes).toBe(130n)
  })
})
