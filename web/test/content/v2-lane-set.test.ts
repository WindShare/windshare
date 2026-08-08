import { describe, expect, it } from 'vitest'

import { FileGeometry } from '../../src/content/geometry'
import {
  V2BlockDispatchSequenceAuthority,
  V2LaneSet,
  type V2BlockDemand,
  type V2BlockDispatchObservation,
  type V2BlockLane,
  type V2BlockRouteObservation,
} from '../../src/content/v2-lane-set'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
import type { V2BlockRouteEligibility } from '../../src/content/v2-route-policy'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'

const ALL_ROUTES: V2BlockRouteEligibility = Object.freeze({
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

const descriptor: V2FileRevisionDescriptor = Object.freeze({
  shareInstance: identity(1),
  shareInstanceId: 'share',
  fileId: identity(2),
  fileIdText: 'file',
  fileRevision: identity(3),
  fileRevisionText: 'revision',
  exactSize: 2n,
  geometry: new FileGeometry(2n, 1n),
})

function demand(index: bigint): V2BlockDemand {
  return { descriptor, leaseId: identity(4), localBlockIndex: index }
}

class DeferredLane implements V2BlockLane {
  readonly id: number
  readonly calls: bigint[] = []
  readonly #pending: Array<(record: V2BlockRecord) => void> = []

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(input: V2BlockDemand): Promise<V2BlockRecord> {
    this.calls.push(input.localBlockIndex)
    return new Promise((resolve) => this.#pending.push(resolve))
  }

  resolve(index: number): void {
    const complete = this.#pending[index]
    if (complete === undefined) throw new Error('missing deferred lane call')
    complete({ descriptor, localBlockIndex: this.calls[index]!, data: Uint8Array.of(this.id) })
  }
}

class CancelThenLane implements V2BlockLane {
  readonly id: number
  calls = 0

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(input: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord> {
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

class ImmediateLane implements V2BlockLane {
  readonly id: number

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(input: V2BlockDemand): Promise<V2BlockRecord> {
    return Promise.resolve({
      descriptor,
      localBlockIndex: input.localBlockIndex,
      data: Uint8Array.of(this.id),
    })
  }
}

describe('v2 receiver LaneSet', () => {
  it('distributes blocks and reports transport provenance only after each fetch succeeds', async () => {
    const observations: V2BlockRouteObservation[] = []
    const dispatches: V2BlockDispatchObservation[] = []
    const lanes = new V2LaneSet({
      onBlockDispatched: (observation) => dispatches.push(observation),
      onBlockFetched: (observation) => observations.push(observation),
    })
    const first = new DeferredLane(1)
    const second = new DeferredLane(2)
    lanes.add(first, 'relay', 0)
    lanes.add(second, 'peer', 9)
    const signal = new AbortController().signal

    const left = lanes.fetch(demand(0n), ALL_ROUTES, signal)
    const right = lanes.fetch(demand(1n), ALL_ROUTES, signal)
    await Promise.resolve()
    expect(first.calls).toEqual([0n])
    expect(second.calls).toEqual([1n])
    expect(dispatches).toEqual([
      {
        dispatchSequence: 1,
        laneId: 1,
        laneEpoch: 0,
        route: 'relay',
        fileId: 'file',
        localBlockIndex: 0n,
      },
      {
        dispatchSequence: 2,
        laneId: 2,
        laneEpoch: 9,
        route: 'peer',
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
        route: 'relay',
        fileId: 'file',
        localBlockIndex: 0n,
      },
      {
        dispatchSequence: 2,
        laneId: 2,
        laneEpoch: 9,
        route: 'peer',
        fileId: 'file',
        localBlockIndex: 1n,
      },
    ])
  })

  it('isolates dispatch observers and assigns sequence before lane invocation', async () => {
    const order: string[] = []
    const lane: V2BlockLane = {
      id: 7,
      fetchBlock: async (input) => {
        order.push('invoke')
        return { descriptor, localBlockIndex: input.localBlockIndex, data: Uint8Array.of(7) }
      },
    }
    const lanes = new V2LaneSet({
      onBlockDispatched: (observation) => {
        order.push(`dispatch-${observation.dispatchSequence}`)
        throw new Error('synthetic dispatch observer failure')
      },
      onBlockFetched: (observation) => order.push(`complete-${observation.dispatchSequence}`),
    })
    lanes.add(lane, 'peer', 11)

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
    const authority = new V2BlockDispatchSequenceAuthority()
    const sequences: number[] = []
    const observe = (observation: V2BlockDispatchObservation) => {
      sequences.push(observation.dispatchSequence)
    }
    const firstGeneration = new V2LaneSet({ dispatchSequence: authority, onBlockDispatched: observe })
    firstGeneration.add(new ImmediateLane(1), 'relay', 0)
    await firstGeneration.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)
    firstGeneration.close()

    const secondGeneration = new V2LaneSet({ dispatchSequence: authority, onBlockDispatched: observe })
    secondGeneration.add(new ImmediateLane(10), 'relay', 0)
    await secondGeneration.fetch(demand(1n), ALL_ROUTES, new AbortController().signal)

    expect(sequences).toEqual([1, 2])
    secondGeneration.close()
  })

  it('waits without polling until content policy admits a lane', async () => {
    const lanes = new V2LaneSet()
    const waiting = lanes.waitForLane()
    lanes.add(new DeferredLane(7), 'relay')
    await expect(waiting).resolves.toBe(7)
  })

  it('does not demote a healthy lane when the caller cancels its request', async () => {
    const lanes = new V2LaneSet()
    const first = new CancelThenLane(1)
    const second = new ImmediateLane(2)
    lanes.add(first, 'relay')
    lanes.add(second, 'peer')
    const controller = new AbortController()
    const cancelled = lanes.fetch(demand(0n), ALL_ROUTES, controller.signal)
    await Promise.resolve()
    controller.abort(new DOMException('consumer left', 'AbortError'))
    await expect(cancelled).rejects.toMatchObject({ name: 'AbortError' })

    expect((await lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).data).toEqual(Uint8Array.of(2))
    expect((await lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).data).toEqual(Uint8Array.of(1))
    expect(first.calls).toBe(2)
  })

  it('adopts a replacement lane that attaches while the failed attempt unwinds', async () => {
    const lanes = new V2LaneSet()
    const replacement = new ImmediateLane(2)
    lanes.add({
      id: 1,
      fetchBlock: async () => {
        lanes.remove(1)
        lanes.add(replacement, 'peer')
        throw new V2SessionRuntimeError('lane', 'detached')
      },
    }, 'relay')

    await expect(lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).resolves.toMatchObject({
      data: Uint8Array.of(2),
    })
  })

  it('does not retry a content-domain failure across healthy physical lanes', async () => {
    const lanes = new V2LaneSet()
    const fallback = new DeferredLane(2)
    lanes.add({
      id: 1,
      fetchBlock: async () => { throw new Error('authenticated block failure') },
    }, 'relay')
    lanes.add(fallback, 'peer')

    await expect(lanes.fetch(demand(0n), ALL_ROUTES, new AbortController().signal)).rejects.toThrow(
      'authenticated block failure',
    )
    expect(fallback.calls).toEqual([])
  })
})
