import { expect, it, vi } from 'vitest'
import { BlockBroker } from '../../src/content/broker'
import { byteRange, FileGeometry } from '../../src/content/geometry'
import { LaneSet } from '../../src/content/lane-set'
import type { BlockRecord } from '../../src/content/records'
import type { BlockRouteEligibility } from '../../src/content/route-policy'
import { deferred } from '../session/v2-send-fixture'

const routes: BlockRouteEligibility = {
  active: true, allows: () => true, assertActive() {}, subscribe: () => () => undefined,
}
const descriptor = {
  shareInstance: new Uint8Array(16), shareInstanceId: 'share',
  fileId: new Uint8Array(16), fileIdText: 'file',
  fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
  exactSize: 2n, geometry: new FileGeometry(2n, 1n),
}
const leaseId = new Uint8Array(16)
const demand = (localBlockIndex: bigint) => ({ descriptor, leaseId, localBlockIndex })
const record = (localBlockIndex: bigint): BlockRecord => ({ descriptor, localBlockIndex, data: Uint8Array.of(7) })

it('withdraws new admission while a healthy lane serves work and the old fetch finishes', async () => {
  const lanes = new LaneSet()
  const first = deferred<BlockRecord>()
  const started = deferred<void>()
  const close = vi.fn()
  const old = vi.fn(() => { started.resolve(); return first.promise })
  lanes.add({ id: 1, fetchBlock: old, close }, 'application-relay', 2)
  const pending = lanes.fetch(demand(0n), routes, new AbortController().signal)
  try {
    await started.promise
    lanes.withdraw(1, 1)
    expect(lanes.size).toBe(1)
    lanes.withdraw(1, 2)
    expect(lanes.size).toBe(0)
    const healthy = vi.fn(async () => record(1n))
    lanes.add({ id: 2, fetchBlock: healthy }, 'direct')
    await expect(lanes.fetch(demand(1n), routes, new AbortController().signal)).resolves.toEqual(record(1n))
    expect(old).toHaveBeenCalledOnce()
    lanes.remove(1)
    expect(close).not.toHaveBeenCalled()
    first.resolve(record(0n))
    await expect(pending).resolves.toEqual(record(0n))
    expect(close).toHaveBeenCalledOnce()
  } finally { first.resolve(record(0n)); lanes.close() }
})

it('keeps buffered range output across retirement and rejects work that needs another network request', async () => {
  const lanes = new LaneSet()
  const first = deferred<BlockRecord>()
  const started = deferred<void>()
  lanes.add({ id: 1, fetchBlock: () => { started.resolve(); return first.promise } }, 'direct')
  const broker = new BlockBroker(lanes)
  const source = broker.readRouteAuthorizedRange(descriptor, leaseId, byteRange(0n, 2n), { routes, maximumParallel: 1 })
  let cleaned = false
  const next = source.next()
  try {
    await started.promise
    lanes.withdraw(1, 0)
    lanes.retire()
    lanes.remove(1)
    const cleanup = Promise.all([lanes.waitForIdle(), broker.waitForIdle()]).then(() => {
      broker.close()
      lanes.close()
      cleaned = true
    })
    first.resolve(record(0n))
    await expect(next).resolves.toMatchObject({ done: false, value: { offset: 0n, data: Uint8Array.of(7) } })
    expect(cleaned).toBe(false)
    await expect(source.next()).rejects.toMatchObject({ scope: 'lane' })
    await cleanup
    expect(cleaned).toBe(true)
  } finally {
    first.resolve(record(0n))
    broker.close()
    lanes.close()
    await source.return(undefined)
  }
})

it('releases metadata requests waiting on a retired generation', async () => {
  const lanes = new LaneSet()
  const request = vi.fn(async () => undefined)
  const pending = lanes.requests.run({ kind: 'list_children' }, request)
  const rejected = expect(pending).rejects.toThrow('Request lanes are closed')
  lanes.retire()
  await rejected
  expect(request).not.toHaveBeenCalled()
  lanes.close()
})

it('explicit broker close releases retirement while an output iterator is suspended', async () => {
  const lanes = new LaneSet()
  lanes.add({ id: 1, fetchBlock: async () => record(0n) }, 'direct')
  const broker = new BlockBroker(lanes)
  const source = broker.readRouteAuthorizedRange(descriptor, leaseId, byteRange(0n, 1n), { routes })
  try {
    expect((await source.next()).done).toBe(false)
    const idle = broker.waitForIdle()
    broker.close()
    await idle
  } finally { broker.close(); lanes.close(); await source.return(undefined) }
})
