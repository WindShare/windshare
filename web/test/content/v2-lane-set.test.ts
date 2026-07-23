import { describe, expect, it } from 'vitest'

import { FileGeometry } from '../../src/content/geometry'
import {
  V2LaneSet,
  type V2BlockDemand,
  type V2BlockLane,
  type V2BlockRouteEligibility,
  type V2BlockRouteObservation,
} from '../../src/content/v2-broker'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
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
    const lanes = new V2LaneSet({ onBlockFetched: (observation) => observations.push(observation) })
    const first = new DeferredLane(1)
    const second = new DeferredLane(2)
    lanes.add(first, 'relay')
    lanes.add(second, 'peer')
    const signal = new AbortController().signal

    const left = lanes.fetch(demand(0n), ALL_ROUTES, signal)
    const right = lanes.fetch(demand(1n), ALL_ROUTES, signal)
    await Promise.resolve()
    expect(first.calls).toEqual([0n])
    expect(second.calls).toEqual([1n])

    first.resolve(0)
    second.resolve(0)
    expect((await left).data).toEqual(Uint8Array.of(1))
    expect((await right).data).toEqual(Uint8Array.of(2))
    expect(observations).toEqual([
      { laneId: 1, route: 'relay', fileId: 'file', localBlockIndex: 0n },
      { laneId: 2, route: 'peer', fileId: 'file', localBlockIndex: 1n },
    ])
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
