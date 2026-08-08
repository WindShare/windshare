import { afterEach, describe, expect, it, vi } from 'vitest'
import { V2LaneSet, type V2BlockLane } from '../../src/content/v2-lane-set'
import type { V2BlockRecord } from '../../src/content/v2-records'
import type {
  V2BrowserConnectivityAttemptDiagnostic,
  V2BrowserSelectedPairDiagnostic,
  V2CandidateCounts,
} from '../../src/connectivity/diagnostics'
import type {
  OfferChannelFactory,
  V2PeerOfferAttemptObserver,
} from '../../src/connectivity/peer-offer'
import type { PeerChannel } from '../../src/connectivity/peer-channel'
import { SIGNAL_KIND_OFFER } from '../../src/connectivity/signaling'
import { V2AuthenticatedPeerOperationError } from '../../src/connectivity/v2-session-signaling'
import {
  type V2ContentLaneAdmissionObservation,
  type V2ContentLaneDetachmentObservation,
  V2ReceiverConnectivity,
  V2_RELAY_CONTENT_FALLBACK_MILLISECONDS,
} from '../../src/connectivity/v2-receiver-policy'
import type { V2SessionMessage } from '../../src/session/v2-message'
import type { V2ReceiverSessionRuntime, V2SessionOperation } from '../../src/session/v2-runtime'
import type {
  V2LaneChange,
  V2OperationCancellation,
} from '../../src/session/v2-runtime-types'

class FakeSession {
  readonly initialLaneId = 1
  readonly keys = Object.freeze({
    protocolSessionId: identity(90),
    initialLaneEpoch: 0,
  })
  readonly #ids = new Set([this.initialLaneId])
  readonly #listeners = new Set<(change: V2LaneChange) => void>()
  attachGate: Promise<void> | undefined
  attachCalls = 0
  grantGate: Promise<void> | undefined
  grantCalls = 0
  readonly #peerOperation = new ControlledSessionOperation()
  #nextLaneId = 2

  laneIds(): readonly number[] {
    return [...this.#ids]
  }

  subscribeLaneChanges(listener: (change: V2LaneChange) => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  async requestLaneGrant(
    _requestedLaneId: number,
    options: { readonly signal?: AbortSignal } = {},
  ) {
    this.grantCalls += 1
    await awaitOptionalGate(this.grantGate, options.signal)
    const laneId = this.#nextLaneId++
    return {
      laneId,
      laneEpoch: 1,
      grantOperationId: identity(laneId),
      attachNonce: identity(laneId + 20),
    }
  }

  async attachGrantedLane(
    _peer: PeerChannel,
    grant: { readonly laneId: number },
    signal?: AbortSignal,
  ): Promise<void> {
    this.attachCalls += 1
    await awaitOptionalGate(this.attachGate, signal)
    signal?.throwIfAborted()
    this.#ids.add(grant.laneId)
  }

  async beginOperation(): Promise<V2SessionOperation> {
    return this.#peerOperation as unknown as V2SessionOperation
  }

  async sendOperationMessage(): Promise<void> {}

  async cancelOperation(
    operation: V2SessionOperation,
    cancellation: V2OperationCancellation,
  ): Promise<void> {
    operation.cancel(cancellation.cause)
  }

  async close(): Promise<void> {}

  failPeerOperation(reason: unknown): void {
    this.#peerOperation.fail(reason)
  }

  detach(laneId: number): void {
    this.#ids.delete(laneId)
    const change: V2LaneChange = { type: 'detached', laneId, laneEpoch: 1 }
    for (const listener of this.#listeners) listener(change)
  }
}

class ControlledSessionOperation {
  readonly #message = deferred<V2SessionMessage>()

  next(): Promise<V2SessionMessage> {
    return this.#message.promise
  }

  fail(reason: unknown): void {
    this.#message.reject(reason)
  }

  cancel(reason: unknown): void {
    this.#message.reject(reason)
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

class ObservedSuccessfulOffers implements OfferChannelFactory {
  calls = 0

  async offer(
    _route: Parameters<OfferChannelFactory['offer']>[0],
    _signal: AbortSignal,
    observer?: V2PeerOfferAttemptObserver,
  ): Promise<PeerChannel> {
    this.calls += 1
    const counts: V2CandidateCounts = Object.freeze({ localEmitted: 1, remoteAccepted: 1 })
    observer?.offerCreated(counts)
    observer?.offerSent(counts)
    observer?.answerReceived(counts)
    observer?.dataChannelOpened(counts, async () => SELECTED_PAIR)
    return fakePeer()
  }
}

class PostOpenSignalingOffers implements OfferChannelFactory {
  async offer(
    route: Parameters<OfferChannelFactory['offer']>[0],
    signal: AbortSignal,
    observer?: V2PeerOfferAttemptObserver,
  ): Promise<PeerChannel> {
    const counts: V2CandidateCounts = Object.freeze({ localEmitted: 1, remoteAccepted: 1 })
    observer?.offerCreated(counts)
    await route.send({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: SIGNAL_KIND_OFFER, sdp: 'v=0\r\ns=post-open-failure\r\n' },
    }, signal)
    observer?.offerSent(counts)
    observer?.answerReceived(counts)
    observer?.dataChannelOpened(counts, async () => SELECTED_PAIR)
    return fakePeer()
  }
}

const SELECTED_PAIR: V2BrowserSelectedPairDiagnostic = Object.freeze({
  candidatePairId: 'pair-1',
  local: Object.freeze({ candidateId: 'local-1', candidateType: 'host', protocol: 'udp' }),
  remote: Object.freeze({ candidateId: 'remote-1', candidateType: 'host', protocol: 'udp' }),
})

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
  readonly reject: (reason: unknown) => void
} {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, decline) => {
    resolve = accept
    reject = decline
  })
  return { promise, resolve, reject }
}

async function awaitOptionalGate(
  gate: Promise<void> | undefined,
  signal: AbortSignal | undefined,
): Promise<void> {
  signal?.throwIfAborted()
  if (gate === undefined) return
  if (signal === undefined) {
    await gate
    return
  }
  await new Promise<void>((resolve, reject) => {
    const abort = () => reject(signal.reason)
    signal.addEventListener('abort', abort, { once: true })
    gate.then(
      () => {
        signal.removeEventListener('abort', abort)
        resolve()
      },
      (error: unknown) => {
        signal.removeEventListener('abort', abort)
        reject(error)
      },
    )
  })
}

function fixture(
  offers: OfferChannelFactory,
  onContentLaneAdmitted?: (observation: V2ContentLaneAdmissionObservation) => void,
  options: {
    readonly nativePeerUsable?: () => boolean
    readonly connectivityObserver?: (diagnostic: V2BrowserConnectivityAttemptDiagnostic) => void
    readonly randomBytes?: (length: number) => Uint8Array
    readonly onContentLaneDetached?: (observation: V2ContentLaneDetachmentObservation) => void
    readonly onPeerError?: (error: unknown) => void
  } = {},
) {
  const session = new FakeSession()
  const lanes = new V2LaneSet()
  const errors: unknown[] = []
  let identitySeed = 7
  const connectivity = new V2ReceiverConnectivity({
    session: session as unknown as V2ReceiverSessionRuntime,
    lanes,
    createBlockLane: (laneId) => new FakeLane(laneId),
    offers,
    randomBytes: options.randomBytes ?? ((length) => new Uint8Array(length).fill(identitySeed++)),
    nativePeerUsable: options.nativePeerUsable ?? (() => true),
    ...(options.connectivityObserver === undefined
      ? {}
      : { connectivityObserver: options.connectivityObserver }),
    ...(options.onContentLaneDetached === undefined
      ? {}
      : { onContentLaneDetached: options.onContentLaneDetached }),
    onPeerError: options.onPeerError ?? ((error) => errors.push(error)),
    ...(onContentLaneAdmitted === undefined ? {} : { onContentLaneAdmitted }),
  })
  return { connectivity, errors, lanes, session }
}

async function turn(): Promise<void> {
  for (let index = 0; index < 20; index += 1) await Promise.resolve()
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

  it('falls back without allocating a binding when the native API is absent', async () => {
    vi.useFakeTimers()
    const offers = new SuccessfulOffers()
    const diagnostics: V2BrowserConnectivityAttemptDiagnostic[] = []
    let randomCalls = 0
    const { connectivity, lanes } = fixture(offers, undefined, {
      nativePeerUsable: () => false,
      randomBytes: (length) => {
        randomCalls += 1
        return new Uint8Array(length).fill(8)
      },
      connectivityObserver: (event) => diagnostics.push(event),
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(offers.calls).toBe(0)
    expect(randomCalls).toBe(0)
    expect(diagnostics).toEqual([])
    expect(lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)

    preview.close()
    await connectivity.close()
  })

  it('contains API-gate and diagnostic observer exceptions without starting a task rejection', async () => {
    vi.useFakeTimers()
    const offers = new SuccessfulOffers()
    const predicateFailure = new Error('synthetic API gate failure')
    let reportedFailures = 0
    const { connectivity, lanes } = fixture(offers, undefined, {
      nativePeerUsable: () => { throw predicateFailure },
      onPeerError: (error) => {
        expect(error).toBe(predicateFailure)
        reportedFailures += 1
        throw new Error('synthetic peer diagnostic failure')
      },
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(offers.calls).toBe(0)
    expect(reportedFailures).toBe(1)
    expect(lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)

    preview.close()
    await expect(connectivity.close()).resolves.toBeUndefined()
  })

  it('emits one schema-valid lifecycle and does not fail it during normal cleanup', async () => {
    vi.useFakeTimers()
    const diagnostics: V2BrowserConnectivityAttemptDiagnostic[] = []
    const { connectivity, lanes } = fixture(new ObservedSuccessfulOffers(), undefined, {
      connectivityObserver: (event) => diagnostics.push(event),
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(lanes.laneIds()).toEqual([2])
    expect(diagnostics.map((event) => event.stage)).toEqual([
      'started',
      'offer-created',
      'offer-sent',
      'answer-received',
      'datachannel-open',
      'lane-granted',
      'lane-attached',
      'admitted',
    ])
    expect(diagnostics.map((event) => event.sideSequence)).toEqual([1, 2, 3, 4, 5, 6, 7, 8])
    expect(diagnostics.at(-1)).toMatchObject({
      stage: 'admitted',
      lane: { laneId: 2, laneEpoch: 1 },
      selectedPair: SELECTED_PAIR,
    })

    preview.close()
    await connectivity.close()
    expect(diagnostics.filter((event) => event.stage === 'failed')).toEqual([])
  })

  it('isolates observer exceptions from authenticated lane admission', async () => {
    vi.useFakeTimers()
    const stages: string[] = []
    const { connectivity, lanes, errors } = fixture(new ObservedSuccessfulOffers(), undefined, {
      connectivityObserver: (event) => {
        stages.push(event.stage)
        throw new Error('synthetic diagnostic observer failure')
      },
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(stages.at(-1)).toBe('admitted')
    expect(lanes.laneIds()).toEqual([2])
    expect(errors).toEqual([])

    preview.close()
    await connectivity.close()
  })

  it('blocks grant, attach, and admission after a post-Open authenticated failure', async () => {
    vi.useFakeTimers()
    const diagnostics: V2BrowserConnectivityAttemptDiagnostic[] = []
    const { connectivity, lanes, session } = fixture(new PostOpenSignalingOffers(), undefined, {
      connectivityObserver: (event) => diagnostics.push(event),
    })
    session.grantGate = new Promise<void>(() => undefined)
    const preview = connectivity.begin('preview')
    await turn()
    expect(session.grantCalls).toBe(1)

    session.failPeerOperation(new V2AuthenticatedPeerOperationError({
      scope: 'peer',
      code: 0x5004,
      retryable: false,
      retryAfterMilliseconds: undefined,
      message: 'sender rejected lane admission',
    }))
    await turn()

    expect(diagnostics.map((event) => event.stage)).toEqual([
      'started',
      'offer-created',
      'offer-sent',
      'answer-received',
      'datachannel-open',
      'failed',
    ])
    expect(diagnostics.at(-1)).toMatchObject({
      failedAtStage: 'lane-granted',
      typedErrorCode: 'peer-admission',
      failureMessage: 'sender rejected lane admission',
      authenticatedSenderOperationFailure: {
        scope: 'peer',
        code: 0x5004,
        message: 'sender rejected lane admission',
      },
    })
    expect(session.attachCalls).toBe(0)
    expect(lanes.laneIds()).toEqual([1])

    preview.close()
    await connectivity.close()
    expect(diagnostics.filter((event) => event.stage === 'failed')).toHaveLength(1)
  })

  it('starts and terminalizes an API-present attempt even when negotiation fails', async () => {
    vi.useFakeTimers()
    const diagnostics: V2BrowserConnectivityAttemptDiagnostic[] = []
    const { connectivity, lanes } = fixture({
      offer: async () => { throw new Error('independent probe did not control this attempt') },
    }, undefined, {
      connectivityObserver: (event) => diagnostics.push(event),
      onPeerError: () => { throw new Error('synthetic peer diagnostic failure') },
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(diagnostics.map((event) => event.stage)).toEqual(['started', 'failed'])
    expect(diagnostics.at(-1)).toMatchObject({
      failedAtStage: 'offer-created',
      failureScope: 'attempt',
      typedErrorCode: 'unexpected',
    })
    expect(lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)

    preview.close()
    await connectivity.close()
    expect(diagnostics.filter((event) => event.stage === 'failed')).toHaveLength(1)
  })

  it('terminalizes a pending attempt exactly once when the runtime stops', async () => {
    vi.useFakeTimers()
    const diagnostics: V2BrowserConnectivityAttemptDiagnostic[] = []
    const { connectivity } = fixture(new PendingOffers(), undefined, {
      connectivityObserver: (event) => diagnostics.push(event),
    })
    connectivity.begin('preview')
    await turn()

    await connectivity.close()

    expect(diagnostics.map((event) => event.stage)).toEqual(['started', 'failed'])
    expect(diagnostics.at(-1)).toMatchObject({
      failedAtStage: 'offer-created',
      typedErrorCode: 'runtime-stopped',
    })
  })
})

describe('v2 receiver fallback and lane policy', () => {
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

  it('observes committed lane admissions without granting the callback policy authority', async () => {
    vi.useFakeTimers()
    const observations: V2ContentLaneAdmissionObservation[] = []
    const visibleLaneIds: number[][] = []
    const admittedLanes: { current?: V2LaneSet } = {}
    const result = fixture(
      new SuccessfulOffers(),
      (observation) => {
        observations.push(observation)
        visibleLaneIds.push([...(admittedLanes.current?.laneIds() ?? [])])
        throw new Error('diagnostic callback failed')
      },
    )
    admittedLanes.current = result.lanes

    const download = result.connectivity.begin('download', 'small')
    await turn()

    expect(observations).toEqual([
      { laneId: 1, laneEpoch: 0, route: 'relay' },
      { laneId: 2, laneEpoch: 1, route: 'peer' },
    ])
    expect(visibleLaneIds).toEqual([[1], [1, 2]])
    expect(observations.every(Object.isFrozen)).toBe(true)
    expect(result.lanes.laneIds()).toEqual([1, 2])
    expect(result.errors).toEqual([])

    download.close()
    await result.connectivity.close()
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
    const detached: V2ContentLaneDetachmentObservation[] = []
    const visibleLaneIds: number[][] = []
    const owner: { lanes?: V2LaneSet } = {}
    const result = fixture(offers, undefined, {
      onContentLaneDetached: (observation) => {
        detached.push(observation)
        visibleLaneIds.push([...(owner.lanes?.laneIds() ?? [])])
        throw new Error('synthetic detach observer failure')
      },
    })
    const { connectivity, lanes, session } = result
    owner.lanes = lanes
    const preview = connectivity.begin('preview')
    await turn()
    expect(lanes.laneIds()).toEqual([2])

    session.detach(2)
    await turn()
    expect(detached).toEqual([{ laneId: 2, laneEpoch: 1, route: 'peer' }])
    expect(visibleLaneIds).toEqual([[]])
    expect(lanes.laneIds()).toEqual([1, 3])
    expect(preview.routes.allows('relay')).toBe(true)
    expect(offers.calls).toBe(2)
    preview.close()
    await connectivity.close()
  })
})
