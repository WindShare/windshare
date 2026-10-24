import { describe, expect, it } from 'vitest'

import { FileGeometry } from '../../src/content/geometry'
import {
  BlockDispatchSequenceAuthority,
  authenticatedBlockRoute,
  LaneSet,
  type BlockDemand,
  type BlockDispatchObservation,
  type BlockLane,
  type BlockRouteObservation,
} from '../../src/content/lane-set'
import type { BlockRecord, FileRevisionDescriptor } from '../../src/content/records'
import type { BlockRouteEligibility } from '../../src/content/route-policy'
import { SessionRuntimeError } from '../../src/session/runtime-types'

const ALL_ROUTES: BlockRouteEligibility = Object.freeze({
  active: true,
  allows: () => true,
  assertActive: () => undefined,
  subscribe: () => () => undefined,
})

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

const descriptor: FileRevisionDescriptor = Object.freeze({
  shareInstance: identity(1),
  shareInstanceId: 'share',
  fileId: identity(2),
  fileIdText: 'file',
  fileRevision: identity(3),
  fileRevisionText: 'revision',
  exactSize: 2n,
  geometry: new FileGeometry(2n, 1n),
})

function demand(index: bigint): BlockDemand {
  return { descriptor, leaseId: identity(4), localBlockIndex: index }
}

class DeferredLane implements BlockLane {
  readonly id: number
  readonly calls: bigint[] = []
  readonly #pending: Array<(record: BlockRecord) => void> = []

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(input: BlockDemand): Promise<BlockRecord> {
    this.calls.push(input.localBlockIndex)
    return new Promise((resolve) => this.#pending.push(resolve))
  }

  resolve(index: number): void {
    const complete = this.#pending[index]
    if (complete === undefined) throw new Error('missing deferred lane call')
    complete({ descriptor, localBlockIndex: this.calls[index]!, data: Uint8Array.of(this.id) })
  }
}

class CancelThenLane implements BlockLane {
  readonly id: number
  calls = 0

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(input: BlockDemand, signal: AbortSignal): Promise<BlockRecord> {
    this.calls += 1
    if (this.calls > 1) {
      return Promise.resolve({
        descriptor,
        localBlockIndex: input.localBlockIndex,
        data: Uint8Array.of(this.id),
      })
    }
    return new Promise((_resolve, reject) => {
      const abort = () => reject(signal.reason)
      signal.addEventListener('abort', abort, { once: true })
    })
  }
}

class ImmediateLane implements BlockLane {
  readonly id: number

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(input: BlockDemand): Promise<BlockRecord> {
    return Promise.resolve({
      descriptor,
      localBlockIndex: input.localBlockIndex,
      data: Uint8Array.of(this.id),
    })
  }
}

describe('v2 receiver LaneSet', () => {
  it('keeps a valid late block but does not attribute traffic to a retired route attestation', async () => {
    const observations: BlockRouteObservation[] = []
    const lanes = new LaneSet({ onBlockFetched: (fact) => observations.push(fact) })
    const direct = new DeferredLane(1)
    lanes.add(direct, 'direct', 2)
    const result = lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)
    for (let turn = 0; turn < 16; turn++) await Promise.resolve()
    expect(lanes.samplingEvidence(1, 2)).toMatchObject({ offeredBytes: 1, settledBytes: 0 })
    lanes.remove(1)
    lanes.add(new ImmediateLane(1), 'turn', 3)
    direct.resolve(0)
    const retiredRecord = await result
    expect(retiredRecord.data).toEqual(Uint8Array.of(1))
    expect(authenticatedBlockRoute(retiredRecord)).toBeUndefined()
    expect(observations).toEqual([])
    expect(lanes.samplingEvidence(1, 2)).toBeUndefined()
    expect(lanes.samplingEvidence(1, 3)).toMatchObject({ offeredBytes: 0, settledBytes: 0, completedBytes: 0 })
    const replacementRecord = await lanes.fetch(demand(1n), ALL_ROUTES, new AbortController().signal)
    expect(authenticatedBlockRoute(replacementRecord)).toBe('turn')
    expect(observations).toMatchObject([{ route: 'turn', laneEpoch: 3, usefulBytes: 1 }])
    expect(lanes.samplingEvidence(1, 3)).toMatchObject({ offeredBytes: 1, settledBytes: 1, completedBytes: 1, completedAttempts: 1 })
    lanes.close()
  })

  it('distributes blocks and reports transport provenance only after each fetch succeeds', async () => {
    const observations: BlockRouteObservation[] = []
    const dispatches: BlockDispatchObservation[] = []
    const lanes = new LaneSet({
      onBlockDispatched: (observation) => dispatches.push(observation),
      onBlockFetched: (observation) => observations.push(observation),
    })
    const first = new DeferredLane(1)
    const second = new DeferredLane(2)
    lanes.add(first, 'direct', 0)
    lanes.add(second, 'application-relay', 9)
    const signal = new AbortController().signal

    const left = lanes.fetch(demand(0n), ALL_ROUTES, signal)
    const right = lanes.fetch(demand(1n), ALL_ROUTES, signal)
    for (let turn = 0; turn < 16; turn++) await Promise.resolve()
    expect(first.calls).toEqual([0n])
    expect(second.calls).toEqual([1n])
    expect(dispatches).toEqual([
      {
        dispatchSequence: 1,
        laneId: 1,
        laneEpoch: 0,
        route: 'direct',
        fileId: 'file',
        localBlockIndex: 0n,
      },
      {
        dispatchSequence: 2,
        laneId: 2,
        laneEpoch: 9,
        route: 'application-relay',
        fileId: 'file',
        localBlockIndex: 1n,
      },
    ])
    expect(observations).toEqual([])

    first.resolve(0)
    second.resolve(0)
    expect((await left).data).toEqual(Uint8Array.of(1))
    expect((await right).data).toEqual(Uint8Array.of(2))
    expect(observations).toEqual([
      {
        dispatchSequence: 1,
        laneId: 1,
        laneEpoch: 0,
        route: 'direct',
        fileId: 'file',
        localBlockIndex: 0n,
        usefulBytes: 1,
      },
      {
        dispatchSequence: 2,
        laneId: 2,
        laneEpoch: 9,
        route: 'application-relay',
        fileId: 'file',
        localBlockIndex: 1n,
        usefulBytes: 1,
      },
    ])
  })

  it('isolates dispatch observers and assigns sequence before lane invocation', async () => {
    const order: string[] = []
    const lane: BlockLane = {
      id: 7,
      fetchBlock: async (input) => {
        order.push('invoke')
        return { descriptor, localBlockIndex: input.localBlockIndex, data: Uint8Array.of(7) }
      },
    }
    const lanes = new LaneSet({
      onBlockDispatched: (observation) => {
        order.push(`dispatch-${observation.dispatchSequence}`)
        throw new Error('synthetic dispatch observer failure')
      },
      onBlockFetched: (observation) => order.push(`complete-${observation.dispatchSequence}`),
    })
    lanes.add(lane, 'direct', 11)

    await expect(lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal))
      .resolves.toMatchObject({ data: Uint8Array.of(7) })
    await expect(lanes.fetch(demand(1n), ALL_ROUTES, new AbortController().signal))
      .resolves.toMatchObject({ data: Uint8Array.of(7) })

    expect(order).toEqual([
      'dispatch-1', 'invoke', 'complete-1',
      'dispatch-2', 'invoke', 'complete-2',
    ])
  })

  it('keeps one dispatch sequence across protocol-generation lane-set replacement', async () => {
    const authority = new BlockDispatchSequenceAuthority()
    const sequences: number[] = []
    const observe = (observation: BlockDispatchObservation) => {
      sequences.push(observation.dispatchSequence)
    }
    const firstGeneration = new LaneSet({ dispatchSequence: authority, onBlockDispatched: observe })
    firstGeneration.add(new ImmediateLane(1), 'application-relay', 0)
    await firstGeneration.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)
    firstGeneration.close()

    const secondGeneration = new LaneSet({ dispatchSequence: authority, onBlockDispatched: observe })
    secondGeneration.add(new ImmediateLane(10), 'application-relay', 0)
    await secondGeneration.fetch(demand(1n), ALL_ROUTES, new AbortController().signal)

    expect(sequences).toEqual([1, 2])
    secondGeneration.close()
  })

  it('waits without polling until content policy admits a lane', async () => {
    const lanes = new LaneSet()
    const waiting = lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)
    lanes.add(new ImmediateLane(7), 'application-relay')
    await expect(waiting).resolves.toMatchObject({ data: Uint8Array.of(7) })
    lanes.close()
  })

  it('does not demote a healthy lane when the caller cancels its request', async () => {
    const lanes = new LaneSet()
    const first = new CancelThenLane(1)
    const second = new ImmediateLane(2)
    lanes.add(first, 'direct')
    lanes.add(second, 'direct')
    const controller = new AbortController()
    const cancelled = lanes.fetch(demand(0n), ALL_ROUTES, controller.signal)
    for (let turn = 0; turn < 16; turn++) await Promise.resolve()
    controller.abort(new DOMException('consumer left', 'AbortError'))
    await expect(cancelled).rejects.toMatchObject({ name: 'AbortError' })
    expect(lanes.samplingEvidence(1, 0)).toMatchObject({ offeredBytes: 1, settledBytes: 1, completedBytes: 0, cancelledAttempts: 1, failedAttempts: 0 })

    expect((await lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).data).toEqual(Uint8Array.of(1))
    lanes.remove(2)
    expect((await lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).data).toEqual(Uint8Array.of(1))
    expect(first.calls).toBe(3)
  })

  it('adopts a replacement lane that attaches while the failed attempt unwinds', async () => {
    const lanes = new LaneSet()
    const replacement = new ImmediateLane(2)
    lanes.add({
      id: 1,
      fetchBlock: async () => {
        lanes.remove(1)
        lanes.add(replacement, 'direct')
        throw new SessionRuntimeError('lane', 'detached')
      },
    }, 'application-relay')

    await expect(lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).resolves.toMatchObject({
      data: Uint8Array.of(2),
    })
  })

  it('does not retry a content-domain failure across healthy physical lanes', async () => {
    const lanes = new LaneSet()
    const fallback = new DeferredLane(2)
    lanes.add({
      id: 1,
      fetchBlock: async () => { throw new Error('authenticated block failure') },
    }, 'direct')
    lanes.add(fallback, 'application-relay')

    await expect(lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).rejects.toThrow(
      'authenticated block failure',
    )
    expect(fallback.calls).toEqual([])
  })
})
