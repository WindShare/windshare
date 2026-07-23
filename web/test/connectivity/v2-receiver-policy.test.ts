import { afterEach, describe, expect, it, vi } from 'vitest'

import { V2LaneSet, type V2BlockLane } from '../../src/content/v2-broker'
import type { V2BlockRecord } from '../../src/content/v2-records'
import type { OfferChannelFactory } from '../../src/connectivity/peer-offer'
import type { PeerChannel } from '../../src/connectivity/peer-channel'
import {
  V2ReceiverConnectivity,
  V2_RELAY_CONTENT_FALLBACK_MILLISECONDS,
} from '../../src/connectivity/v2-receiver-policy'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import type { V2LaneChange } from '../../src/session/v2-runtime-types'

class FakeSession {
  readonly initialLaneId = 1
  readonly #ids = new Set([this.initialLaneId])
  readonly #listeners = new Set<(change: V2LaneChange) => void>()
  attachGate: Promise<void> | undefined
  attachCalls = 0
  #nextLaneId = 2

  laneIds(): readonly number[] {
    return [...this.#ids]
  }

  subscribeLaneChanges(listener: (change: V2LaneChange) => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  async requestLaneGrant() {
    const laneId = this.#nextLaneId++
    return {
      laneId,
      laneEpoch: 1,
      grantOperationId: identity(laneId),
      attachNonce: identity(laneId + 20),
    }
  }

  async attachGrantedLane(_peer: PeerChannel, grant: { readonly laneId: number }): Promise<void> {
    this.attachCalls += 1
    await this.attachGate
    this.#ids.add(grant.laneId)
  }

  detach(laneId: number): void {
    this.#ids.delete(laneId)
    const change: V2LaneChange = { type: 'detached', laneId, laneEpoch: 1 }
    for (const listener of this.#listeners) listener(change)
  }
}

class FakeLane implements V2BlockLane {
  readonly id: number

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(): Promise<V2BlockRecord> {
    return Promise.reject(new Error('not used by connectivity policy tests'))
  }
}

class PendingOffers implements OfferChannelFactory {
  calls = 0

  offer(_route: Parameters<OfferChannelFactory['offer']>[0], signal: AbortSignal): Promise<PeerChannel> {
    this.calls += 1
    return new Promise((_resolve, reject) => {
      const abort = () => reject(signal.reason)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
    })
  }
}

class SuccessfulOffers implements OfferChannelFactory {
  calls = 0

  async offer(): Promise<PeerChannel> {
    this.calls += 1
    return fakePeer()
  }
}

class DeferredSuccessfulOffers implements OfferChannelFactory {
  calls = 0
  readonly #peer = deferred<PeerChannel>()

  offer(): Promise<PeerChannel> {
    this.calls += 1
    return this.#peer.promise
  }

  succeed(): void {
    this.#peer.resolve(fakePeer())
  }
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function fakePeer(): PeerChannel {
  return {
    state: 'open',
    frames: new ReadableStream<Uint8Array>(),
    opened: Promise.resolve(),
    done: Promise.resolve(),
    reason: undefined,
    send: async () => undefined,
    sendTerminal: async () => undefined,
    close: async () => undefined,
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accept) => { resolve = accept })
  return { promise, resolve }
}

function fixture(offers: OfferChannelFactory) {
  const session = new FakeSession()
  const lanes = new V2LaneSet()
  const errors: unknown[] = []
  const connectivity = new V2ReceiverConnectivity({
    session: session as unknown as V2ReceiverSessionRuntime,
    lanes,
    createBlockLane: (laneId) => new FakeLane(laneId),
    offers,
    randomBytes: (length) => new Uint8Array(length).fill(7),
    onPeerError: (error) => errors.push(error),
  })
  return { connectivity, errors, lanes, session }
}

async function turn(): Promise<void> {
  for (let index = 0; index < 6; index += 1) await Promise.resolve()
}

afterEach(() => vi.useRealTimers())

describe('v2 receiver content activation policy', () => {
  it('does no peer or fallback work while the receiver only browses', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)
    expect(offers.calls).toBe(0)
    expect(vi.getTimerCount()).toBe(0)
    expect(lanes.size).toBe(0)
    await connectivity.close()
  })

  it('records preview timing at begin and admits relay at exactly eight seconds', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)
    const preview = connectivity.begin('preview')
    expect(offers.calls).toBe(1)
    expect(lanes.size).toBe(0)
    expect(preview.routes.allows('relay')).toBe(false)
    await vi.advanceTimersByTimeAsync(V2_RELAY_CONTENT_FALLBACK_MILLISECONDS - 1)
    expect(lanes.size).toBe(0)
    expect(preview.routes.allows('relay')).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    expect(lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)
    preview.close()
    expect(preview.routes.active).toBe(false)
    await connectivity.close()
  })

  it.each(['large', 'unknown'] as const)(
    'starts download P2P at click time and preserves the eight-second relay deadline for %s size',
    async (sizeClass) => {
      vi.useFakeTimers()
      const offers = new PendingOffers()
      const { connectivity, lanes } = fixture(offers)

      const download = connectivity.begin('download', sizeClass)
      expect(offers.calls).toBe(1)
      expect(lanes.size).toBe(0)
      expect(download.routes.allows('relay')).toBe(false)
      await vi.advanceTimersByTimeAsync(V2_RELAY_CONTENT_FALLBACK_MILLISECONDS - 1)
      expect(lanes.size).toBe(0)
      expect(download.routes.allows('relay')).toBe(false)
      await vi.advanceTimersByTimeAsync(1)
      expect(lanes.laneIds()).toEqual([1])
      expect(download.routes.allows('relay')).toBe(true)

      download.close()
      await connectivity.close()
    },
  )

  it('keeps the admitted relay lane usable throughout peer lane grant and attach', async () => {
    vi.useFakeTimers()
    const offers = new DeferredSuccessfulOffers()
    const { connectivity, lanes, session } = fixture(offers)
    const attach = deferred<void>()
    session.attachGate = attach.promise

    const download = connectivity.begin('download', 'unknown')
    expect(offers.calls).toBe(1)
    await vi.advanceTimersByTimeAsync(V2_RELAY_CONTENT_FALLBACK_MILLISECONDS)
    expect(lanes.laneIds()).toEqual([1])

    offers.succeed()
    await turn()
    expect(session.attachCalls).toBe(1)
    expect(lanes.laneIds()).toEqual([1])

    attach.resolve()
    await turn()
    expect(lanes.laneIds()).toEqual([1, 2])

    download.close()
    await connectivity.close()
  })

  it('keeps preview and download fallback cancellation independent', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)
    const preview = connectivity.begin('preview')
    await vi.advanceTimersByTimeAsync(4_000)
    const download = connectivity.begin('download', 'large')
    preview.close()
    await vi.advanceTimersByTimeAsync(3_999)
    expect(lanes.size).toBe(0)
    await vi.advanceTimersByTimeAsync(4_001)
    expect(lanes.laneIds()).toEqual([1])
    expect(offers.calls).toBe(1)
    download.close()
    await connectivity.close()
  })

  it('does not leak a prior small activation relay grant into a later large activation', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)

    const small = connectivity.begin('download', 'small')
    expect(lanes.laneIds()).toEqual([1])
    expect(lanes.eligibleSize(small.routes)).toBe(1)
    small.close()

    const large = connectivity.begin('download', 'large')
    expect(lanes.laneIds()).toEqual([1])
    expect(lanes.eligibleSize(large.routes)).toBe(0)
    await vi.advanceTimersByTimeAsync(V2_RELAY_CONTENT_FALLBACK_MILLISECONDS - 1)
    expect(lanes.eligibleSize(large.routes)).toBe(0)
    await vi.advanceTimersByTimeAsync(1)
    expect(lanes.eligibleSize(large.routes)).toBe(1)

    large.close()
    await connectivity.close()
  })

  it('keeps overlapping small and large relay authority independent', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)

    const small = connectivity.begin('download', 'small')
    const large = connectivity.begin('download', 'large')
    expect(lanes.eligibleSize(small.routes)).toBe(1)
    expect(lanes.eligibleSize(large.routes)).toBe(0)
    await vi.advanceTimersByTimeAsync(V2_RELAY_CONTENT_FALLBACK_MILLISECONDS - 1)
    expect(lanes.eligibleSize(large.routes)).toBe(0)
    await vi.advanceTimersByTimeAsync(1)
    expect(lanes.eligibleSize(large.routes)).toBe(1)

    small.close()
    large.close()
    await connectivity.close()
  })

  it('starts a replacement P2P attempt when a new click races old-attempt cleanup', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity } = fixture(offers)
    const first = connectivity.begin('preview')
    first.close()
    const replacement = connectivity.begin('preview')
    expect(offers.calls).toBe(1)
    await turn()
    expect(offers.calls).toBe(2)
    replacement.close()
    await connectivity.close()
  })

  it('admits relay immediately for small downloads and explicit P2P failure', async () => {
    vi.useFakeTimers()
    const pending = fixture(new PendingOffers())
    const small = pending.connectivity.begin('download', 'small')
    expect(pending.lanes.laneIds()).toEqual([1])
    expect(small.routes.allows('relay')).toBe(true)
    small.close()
    await pending.connectivity.close()

    const failures = fixture({
      offer: async () => { throw new Error('P2P unavailable') },
    })
    const preview = failures.connectivity.begin('preview')
    await turn()
    expect(failures.lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)
    expect(failures.errors).toHaveLength(1)
    preview.close()
    await failures.connectivity.close()
  })

  it('hot-switches to relay and starts a replacement peer when a peer lane detaches', async () => {
    vi.useFakeTimers()
    const offers = new SuccessfulOffers()
    const { connectivity, lanes, session } = fixture(offers)
    const preview = connectivity.begin('preview')
    await turn()
    expect(lanes.laneIds()).toEqual([2])

    session.detach(2)
    await turn()
    expect(lanes.laneIds()).toEqual([1, 3])
    expect(preview.routes.allows('relay')).toBe(true)
    expect(offers.calls).toBe(2)
    preview.close()
    await connectivity.close()
  })
})
