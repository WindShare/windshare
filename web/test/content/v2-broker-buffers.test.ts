import { describe, expect, it } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { BlockBroker, type BlockBrokerOptions } from '../../src/content/broker'
import { LaneSet, type BlockDemand } from '../../src/content/lane-set'
import type { BlockRouteEligibility } from '../../src/content/route-policy'
import { IMMEDIATE_BLOCK_CONSUMER } from '../../src/content/scheduling/range-window'

const MIB = 1024 * 1024
const BLOCK_BYTES = 4 * MIB
const routes: BlockRouteEligibility = {
  active: true, allows: () => true, assertActive() {}, subscribe: () => () => {},
}
const descriptor = {
  shareInstance: new Uint8Array(16), shareInstanceId: 'share',
  fileId: new Uint8Array(16), fileIdText: 'file',
  fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
  exactSize: BigInt(2 * BLOCK_BYTES),
  geometry: new FileGeometry(BigInt(2 * BLOCK_BYTES), BigInt(BLOCK_BYTES)),
}
const demand = (index = 0n): BlockDemand => ({
  descriptor, leaseId: new Uint8Array(16), localBlockIndex: index,
})
function setup(options: BlockBrokerOptions) {
  const lanes = new LaneSet({ now: () => 1 })
  const calls: bigint[] = []
  lanes.add({ id: 1, fetchBlock: async input => {
    calls.push(input.localBlockIndex)
    return { descriptor: input.descriptor, localBlockIndex: input.localBlockIndex,
      data: new Uint8Array(BLOCK_BYTES).fill(Number(input.localBlockIndex) + 1) }
  } }, 'direct')
  const broker = new BlockBroker(lanes, options)
  return { broker, calls, close: () => { broker.close(); lanes.close() } }
}
async function flush() {
  for (let turn = 0; turn < 32; turn++) await Promise.resolve()
}

describe('broker cache policy and transfer capacity', () => {
  it.each([0, MIB, BLOCK_BYTES])('reads supported blocks with a %i byte retained-cache limit', async maximumRetainedCacheBytes => {
    const { broker, calls, close } = setup({ maximumRetainedCacheBytes })
    try {
      const first = await broker.readBlock(demand(), { routes })
      expect(first.data.byteLength).toBe(BLOCK_BYTES)
      first.data[0] = 99
      const second = await broker.readBlock(demand(), { routes })
      expect(second.data[0]).toBe(1)
      expect(calls).toHaveLength(maximumRetainedCacheBytes < BLOCK_BYTES ? 2 : 1)
    } finally { close() }
  })

  it('reads at the minimum working capacity even with cache disabled', async () => {
    const { broker, calls, close } = setup({
      maximumBlockBufferBytes: 2 * BLOCK_BYTES, maximumRetainedCacheBytes: 0,
    })
    try {
      const first = await broker.readBlock(demand(), { routes })
      const next = await broker.readBlock(demand(1n), { routes })
      expect(first.data.byteLength).toBe(BLOCK_BYTES)
      expect(next.data[0]).toBe(2)
      expect(calls).toEqual([0n, 1n])
    } finally { close() }
  })

  it('reports an insufficient transfer budget before dispatch, independently of cache size', async () => {
    const { broker, calls, close } = setup({
      maximumBlockBufferBytes: BLOCK_BYTES, maximumRetainedCacheBytes: 64 * MIB,
    })
    try {
      await expect(broker.readBlock(demand(), { routes })).rejects
        .toThrow('Block transfer needs 8388608 reserved bytes; block buffer capacity is 4194304')
      expect(calls).toEqual([])
    } finally { close() }
  })

  it('lets active transfers reclaim cached space without exceeding the common pool', async () => {
    const { broker, calls, close } = setup({
      maximumBlockBufferBytes: 2 * BLOCK_BYTES, maximumRetainedCacheBytes: 2 * BLOCK_BYTES,
    })
    try {
      await broker.readBlock(demand(), { routes })
      await broker.readBlock(demand(), { routes })
      expect(calls).toEqual([0n])
      await broker.readBlock(demand(1n), { routes })
      await broker.readBlock(demand(), { routes })
      expect(calls).toEqual([0n, 1n, 0n])
    } finally { close() }
  })

  it('preserves shared results until the last consumer releases them, then wakes waiting transfers', async () => {
    const { broker, calls, close } = setup({
      maximumBlockBufferBytes: 2 * BLOCK_BYTES, maximumRetainedCacheBytes: 0,
    })
    let resume!: () => void
    const ready = new Promise<void>(resolve => { resume = resolve })
    try {
      const first = broker.readBlock(demand(), { routes })
      const paused = broker.readBlock(demand(), { routes,
        consumer: { ...IMMEDIATE_BLOCK_CONSUMER, ready: () => ready } })
      await first
      const next = broker.readBlock(demand(1n), { routes })
      await flush()
      expect(calls).toEqual([0n])
      resume()
      expect((await paused).data.byteLength).toBe(BLOCK_BYTES)
      expect((await next).data[0]).toBe(2)
      expect(calls).toEqual([0n, 1n])
    } finally { resume(); close() }
  })
})
