import { describe, expect, it } from 'vitest'
import { StagingBudgetCoordinator } from '../../src/output/staging-budget/coordinator'
import { withStagingExportAuthority } from '../../src/output/staging-budget/export-authority'
import type { StagingBudgetInventory, StagingBudgetMutation, StagingBudgetRecord, StagingBudgetStore } from '../../src/output/staging-budget/contracts'
import type { BrowserDeliveryMaterializationOptions } from '../../src/output/browser-delivery/session'
import { checkpointBytes } from '../../src/output/browser-delivery/engine'
import { stageCheckpoint } from '../../src/output/browser-delivery/lifecycle'
import { deliveryPolicy } from './browser-delivery-fixture'
import { deliveryEngineFixture } from './browser-delivery-engine-fixture'

class CapacityStore implements StagingBudgetStore {
  readonly coordinationScope = 'context' as const
  records: StagingBudgetRecord[] = []
  async transact<T>(update: (inventory: StagingBudgetInventory) => StagingBudgetMutation<T>): Promise<T> {
    const mutation = update({ records: this.records, workspace: { occupiedBytes: 0n, outstandingBytes: 0n } })
    for (const record of [...mutation.puts ?? [], ...mutation.put === undefined ? [] : [mutation.put]]) {
      this.records = [...this.records.filter(value => value.id !== record.id), record]
    }
    const deleted = new Set([...mutation.deleteIds ?? [], ...mutation.deleteId === undefined ? [] : [mutation.deleteId]])
    this.records = this.records.filter(record => !deleted.has(record.id))
    return mutation.result
  }
}

async function fixture() {
  const store = new CapacityStore()
  const operationId = deliveryPolicy().operationId
  const operationLease = { operationId, leaseId: 'exclusive-original-operation' }
  const coordinator = new StagingBudgetCoordinator({ store, storage: async () => ({ opfs: 'usable', persistence: 'not-persisted',
    quota: { kind: 'estimated', quotaBytes: 10_000_000_000n, usageBytes: 0n }, pressure: 'normal' }) })
  const reserveStage: BrowserDeliveryMaterializationOptions['reserveStage'] = async (source, retained) => {
    if (retained === undefined) {
      const decision = await coordinator.tryReserve({ operationId, fileId: source.fileId, exactSize: source.exactSize })
      return decision.kind === 'admitted' ? decision.reservation : undefined
    }
    const state = retained.state
    const checkpoint = state.kind === 'receiving' || state.kind === 'discarding' ? state.checkpoint : stageCheckpoint(state)
    let phase: 'receiving' | 'target-saved' | 'queued' = 'queued'
    if (state.kind === 'receiving' || state.kind === 'discarding') phase = 'receiving'
    else if (state.kind === 'target-saved' || state.kind === 'cleanup-pending') phase = 'target-saved'
    return coordinator.restore({ operationId, fileId: source.fileId, exactSize: source.exactSize, operationLease,
      verifiedStagedBytes: checkpointBytes(checkpoint), phase })
  }
  const delivery = await deliveryEngineFixture({ reserveStage, reconcileReservations: async deliveryFileIds => {
    await coordinator.reconcileUnused({ operationId, deliveryFileIds, operationLease })
  } })
  return { ...delivery, store, coordinator, operationId }
}

describe('staging capacity wired through actual delivery sessions', () => {
  it('cancels a never-used claim when durable file placement fails with a confirmed absent record', async () => {
    const f = await fixture()
    f.repository.failCreate = true
    await expect(f.session.beginFile(f.request())).rejects.toThrow('delivery creation failed')
    expect(f.store.records).toHaveLength(0)
    expect(f.repository.files.size).toBe(0)
    await f.session.close()
  })

  it('reconciles a process loss between capacity and file-record commits before a reopened task admits files', async () => {
    const f = await fixture()
    const source = await f.request().openRevision()
    expect(await f.coordinator.tryReserve({ operationId: f.operationId, fileId: source.fileId, exactSize: source.exactSize })).toMatchObject({ kind: 'admitted' })
    expect(f.store.records).toHaveLength(1)
    await f.session.close()
    const reopened = await f.reopen()
    expect(f.store.records).toHaveLength(0)
    const file = await reopened.beginFile(f.request())
    await file.writeRange(0n, Uint8Array.of(1, 2, 3, 4))
    await file.commit()
    expect(reopened.summary.targetSavedBytes).toBe(4n)
    expect(f.store.records).toHaveLength(0)
    await reopened.close()
  })

  it('saves an unrelated task after a vanished exporter while retaining the first task staging obligation', async () => {
    const f = await fixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, Uint8Array.of(4, 3, 2, 1))
    const other = await f.coordinator.tryReserve({ operationId: 'other-operation', fileId: 'other-file', exactSize: 4n })
    if (other.kind !== 'admitted') throw new Error(other.reason)
    await other.reservation.received(4n)
    await other.reservation.queueExport()
    await withStagingExportAuthority(async authority => { expect(await other.reservation.beginExport(authority)).toBe(true) })
    expect(f.store.records.find(record => record.operationId === 'other-operation')?.phase).toBe('exporting')
    await file.commit()
    expect(f.session.summary.targetSavedBytes).toBe(4n)
    expect(f.store.records).toHaveLength(1)
    expect(f.store.records[0]).toMatchObject({ operationId: 'other-operation', phase: 'export-failed', verifiedStagedBytes: 4n })
    await f.session.close()
  })
})
