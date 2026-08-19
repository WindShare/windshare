import { afterEach, describe, expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { V2LaneSet, type V2BlockDemand, type V2BlockLane } from '../../src/content/v2-lane-set'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
import { createProtocolFailure, type ProtocolFailure } from '../../src/diagnostics/incident/fact'
import type {
  V2CandidateCounts,
  V2PeerAttemptTraceEvent,
  V2PeerRecoveryTraceEvent,
} from '../../src/connectivity/diagnostics'
import type {
  OfferChannelFactory,
  V2PeerOfferAttemptObserver,
} from '../../src/connectivity/peer-offer'
import type { PeerChannel } from '../../src/connectivity/peer-channel'
import { PeerNegotiationError } from '../../src/connectivity/errors'
import { SIGNAL_KIND_OFFER } from '../../src/connectivity/signaling'
import {
  V2AuthenticatedPeerOperationError,
  V2SessionSignalingRoute,
} from '../../src/connectivity/v2-session-signaling'
import {
  type V2ContentLaneAdmissionObservation,
  type V2ContentLaneDetachmentObservation,
  V2ReceiverConnectivity,
} from '../../src/connectivity/v2-receiver-policy'
import type { V2SessionMessage } from '../../src/session/v2-message'
import type {
  V2ReceiverSessionRuntime,
  V2SessionOperation,
} from '../../src/session/v2-runtime'
import {
  createV2ProtocolOperationIdentity,
  createV2ProtocolSessionIdentity,
} from '../../src/session/v2-identities'
import type {
  V2LaneChange,
  V2OperationCancellation,
} from '../../src/session/v2-runtime-types'
import type {
  V2PeerRecoveryClock,
  V2PeerRecoveryDependencies,
} from '../../src/connectivity/v2-peer-recovery'

class FakeSession {
  readonly initialLaneId = 1
  readonly keys = Object.freeze({
    protocolSessionId: identity(90),
    initialLaneEpoch: 0,
  })
  readonly protocolSessionIdentity = createV2ProtocolSessionIdentity(this.keys.protocolSessionId)
  readonly protocolFailures: ProtocolFailure[] = []
  readonly #ids = new Set([this.initialLaneId])
  readonly #listeners = new Set<(change: V2LaneChange) => void>()
  attachGate: Promise<void> | undefined
  attachCalls = 0
  grantGate: Promise<void> | undefined
  grantCalls = 0
  readonly requestedLaneIds: number[] = []
  #peerOperation: ControlledSessionOperation | undefined
  #nextGrantOperationIdentity = 40
  #nextPeerOperationIdentity = 80
  #nextLaneId = 2
  closeCalls = 0

  laneIds(): readonly number[] {
    return [...this.#ids]
  }

  subscribeLaneChanges(listener: (change: V2LaneChange) => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  async requestLaneGrant(
    requestedLaneId: number,
    options: { readonly signal?: AbortSignal } = {},
  ) {
    this.grantCalls += 1
    this.requestedLaneIds.push(requestedLaneId)
    const laneId = requestedLaneId === 0 ? this.#nextLaneId++ : requestedLaneId
    const operationIdentity = identity(this.#nextGrantOperationIdentity++)
    await awaitOptionalGate(this.grantGate, options.signal)
    const grant = {
      laneId,
      laneEpoch: requestedLaneId === 0 ? 1 : 2,
      grantOperationId: operationIdentity,
      attachNonce: identity(this.#nextGrantOperationIdentity + 40),
    }
    return grant
  }

  async adoptGrantedLane(
    _peer: PeerChannel,
    grant: {
      readonly laneId: number
      readonly laneEpoch: number
      readonly grantOperationId: Uint8Array
    },
    options: { readonly signal?: AbortSignal } = {},
  ) {
    this.attachCalls += 1
    const correlation = {
      grantOperationId: createV2ProtocolOperationIdentity(grant.grantOperationId),
      laneId: grant.laneId,
      laneEpoch: grant.laneEpoch,
    }
    await awaitOptionalGate(this.attachGate, options.signal)
    options.signal?.throwIfAborted()
    this.#ids.add(grant.laneId)
    const change: V2LaneChange = {
      type: 'attached',
      laneId: grant.laneId,
      laneEpoch: grant.laneEpoch,
    }
    for (const listener of this.#listeners) listener(change)
    return Object.freeze({
      ...correlation,
      disposition: 'accepted' as const,
      installation: 'installed' as const,
    })
  }

  async beginOperation(): Promise<V2SessionOperation> {
    const operation = new ControlledSessionOperation(identity(this.#nextPeerOperationIdentity++))
    this.#peerOperation = operation
    return operation as unknown as V2SessionOperation
  }

  async sendOperationMessage(): Promise<void> {}

  async cancelOperation(
    operation: V2SessionOperation,
    cancellation: V2OperationCancellation,
  ): Promise<void> {
    operation.cancel(cancellation.cause)
  }

  operationCorrelation(operation: V2SessionOperation) {
    return Object.freeze({
      protocolSessionId: this.protocolSessionIdentity,
      protocolOperationId: createV2ProtocolOperationIdentity(operation.id),
      lane: Object.freeze({ id: this.initialLaneId, epoch: this.keys.initialLaneEpoch }),
    })
  }

  recordProtocolFailure(failure: ProtocolFailure): void {
    this.protocolFailures.push(failure)
  }

  async close(): Promise<void> {
    this.closeCalls += 1
  }

  failPeerOperation(reason: unknown): void {
    if (this.#peerOperation === undefined) throw new Error('peer operation is not active')
    this.#peerOperation.fail(reason)
  }

  detach(laneId: number): void {
    this.#ids.delete(laneId)
    const change: V2LaneChange = { type: 'detached', laneId, laneEpoch: 1 }
    for (const listener of this.#listeners) listener(change)
  }
}

class ControlledSessionOperation {
  readonly id: Uint8Array<ArrayBuffer>
  readonly #message = deferred<V2SessionMessage>()

  constructor(id: Uint8Array<ArrayBuffer>) {
    this.id = id
  }

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

  fetchBlock(input: V2BlockDemand): Promise<V2BlockRecord> {
    return Promise.resolve({
      descriptor: input.descriptor,
      localBlockIndex: input.localBlockIndex,
      data: Uint8Array.of(this.id),
    })
  }
}

class PendingOffers implements OfferChannelFactory {
  calls = 0
  aborts = 0
  readonly #onOffer: () => void

  constructor(onOffer: () => void = () => undefined) {
    this.#onOffer = onOffer
  }

  offer(_route: Parameters<OfferChannelFactory['offer']>[0], signal: AbortSignal): Promise<PeerChannel> {
    this.calls += 1
    this.#onOffer()
    return new Promise((_resolve, reject) => {
      const abort = () => {
        this.aborts += 1
        reject(signal.reason)
      }
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
    })
  }
}

class SuccessfulOffers implements OfferChannelFactory {
  calls = 0

  async offer(
    route: Parameters<OfferChannelFactory['offer']>[0],
    signal: AbortSignal,
    observer?: V2PeerOfferAttemptObserver,
  ): Promise<PeerChannel> {
    this.calls += 1
    await completeSuccessfulSignaling(route, signal, observer)
    observer?.dataChannelOpened(CANDIDATE_COUNTS)
    return fakePeer()
  }
}

class GatedDetachedPeerCleanupOffers implements OfferChannelFactory {
  calls = 0
  peerCloseCalls = 0
  routeCloseCalls = 0
  readonly #peerCloseGate = deferred<void>()
  readonly #routeCloseGate = deferred<void>()

  async offer(
    route: Parameters<OfferChannelFactory['offer']>[0],
    signal: AbortSignal,
    observer?: V2PeerOfferAttemptObserver,
  ): Promise<PeerChannel> {
    this.calls += 1
    if (this.calls > 1) return pendingPeer(signal)
    await completeSuccessfulSignaling(route, signal, observer)
    observer?.dataChannelOpened(CANDIDATE_COUNTS)
    if (!(route instanceof V2SessionSignalingRoute)) {
      throw new Error('Expected the receiver connectivity signaling route')
    }
    const closeRoute = route.close.bind(route)
    route.close = async (cause?: unknown) => {
      this.routeCloseCalls += 1
      await this.#routeCloseGate.promise
      await closeRoute(cause)
    }
    return {
      ...fakePeer(),
      close: async () => {
        this.peerCloseCalls += 1
        await this.#peerCloseGate.promise
      },
    }
  }

  releasePeerClose(): void {
    this.#peerCloseGate.resolve()
  }

  releaseRouteClose(): void {
    this.#routeCloseGate.resolve()
  }
}

const CANDIDATE_COUNTS: V2CandidateCounts = Object.freeze({
  localEmitted: 1,
  remoteAccepted: 1,
})

async function completeSuccessfulSignaling(
  route: Parameters<OfferChannelFactory['offer']>[0],
  signal: AbortSignal,
  observer?: V2PeerOfferAttemptObserver,
): Promise<void> {
  observer?.offerCreated(CANDIDATE_COUNTS)
  await route.send({
    kind: SIGNAL_KIND_OFFER,
    payload: { type: SIGNAL_KIND_OFFER, sdp: 'v=0\r\ns=test-offer\r\n' },
  }, signal)
  observer?.offerSent(CANDIDATE_COUNTS)
  observer?.answerReceived(CANDIDATE_COUNTS)
}

class DeferredSuccessfulOffers implements OfferChannelFactory {
  calls = 0
  readonly #peer = deferred<PeerChannel>()

  async offer(
    route: Parameters<OfferChannelFactory['offer']>[0],
    signal: AbortSignal,
    observer?: V2PeerOfferAttemptObserver,
  ): Promise<PeerChannel> {
    this.calls += 1
    await completeSuccessfulSignaling(route, signal, observer)
    const peer = await this.#peer.promise
    observer?.dataChannelOpened(CANDIDATE_COUNTS)
    return peer
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

function authenticatedPeerFailure(code: number): V2AuthenticatedPeerOperationError {
  return new V2AuthenticatedPeerOperationError(createProtocolFailure({
    requestKind: 'peer_offer',
    wireScope: 'peer',
    wireCode: code,
    retryable: false,
    settlement: Object.freeze({ kind: 'received_authenticated' }),
    correlation: Object.freeze({
      protocolSessionId: createV2ProtocolSessionIdentity(identity(90)),
      protocolOperationId: createV2ProtocolOperationIdentity(identity(80)),
    }),
  }))
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

function pendingPeer(signal: AbortSignal): Promise<PeerChannel> {
  return new Promise((_resolve, reject) => {
    const abort = () => reject(signal.reason)
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
  })
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
    readonly attemptTraceObserver?: (event: V2PeerAttemptTraceEvent) => void
    readonly recoveryTraceObserver?: (event: V2PeerRecoveryTraceEvent) => void
    readonly randomBytes?: (length: number) => Uint8Array
    readonly onContentLaneDetached?: (observation: V2ContentLaneDetachmentObservation) => void
    readonly peerRecovery?: V2PeerRecoveryDependencies
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
    ...(options.attemptTraceObserver === undefined && options.recoveryTraceObserver === undefined
      ? {}
      : {
          connectivityTrace: {
            current: (event) => {
              if (event.eventName === 'peer_attempt') options.attemptTraceObserver?.(event)
              else options.recoveryTraceObserver?.(event)
            },
          },
        }),
    ...(options.onContentLaneDetached === undefined
      ? {}
      : { onContentLaneDetached: options.onContentLaneDetached }),
    ...(options.peerRecovery === undefined ? {} : { peerRecovery: options.peerRecovery }),
    ...(onContentLaneAdmitted === undefined ? {} : { onContentLaneAdmitted }),
  })
  return { connectivity, errors, lanes, session }
}

async function turn(): Promise<void> {
  for (let index = 0; index < 20; index += 1) await Promise.resolve()
}

async function waitForMicrotaskCondition(
  description: string,
  condition: () => boolean,
): Promise<void> {
  for (let index = 0; index < 100; index += 1) {
    if (condition()) return
    await Promise.resolve()
  }
  throw new Error(`Timed out waiting for ${description}`)
}

class ManualRecoveryClock implements V2PeerRecoveryClock {
  #now = 0
  readonly #pending = new Set<{
    readonly deadline: number
    readonly resolve: () => void
    readonly reject: (reason: unknown) => void
    readonly signal: AbortSignal
    readonly aborted: () => void
  }>()

  now(): number {
    return this.#now
  }

  sleep(milliseconds: number, signal: AbortSignal): Promise<void> {
    if (signal.aborted) return Promise.reject(signal.reason)
    return new Promise((resolve, reject) => {
      const pending = {
        deadline: this.#now + milliseconds,
        resolve,
        reject,
        signal,
        aborted: () => {
          this.#pending.delete(pending)
          reject(signal.reason)
        },
      }
      signal.addEventListener('abort', pending.aborted, { once: true })
      this.#pending.add(pending)
    })
  }

  async advance(milliseconds: number): Promise<void> {
    this.#now += milliseconds
    for (const pending of [...this.#pending]) {
      if (pending.deadline > this.#now) continue
      this.#pending.delete(pending)
      pending.signal.removeEventListener('abort', pending.aborted)
      pending.resolve()
    }
    await turn()
  }
}

const RELAY_DESCRIPTOR: V2FileRevisionDescriptor = Object.freeze({
  shareInstance: identity(51),
  shareInstanceId: 'share',
  fileId: identity(52),
  fileIdText: 'file',
  fileRevision: identity(53),
  fileRevisionText: 'revision',
  exactSize: 1n,
  geometry: new FileGeometry(1n, 1n),
})

function relayDemand(): V2BlockDemand {
  return {
    descriptor: RELAY_DESCRIPTOR,
    leaseId: identity(54),
    localBlockIndex: 0n,
  }
}

afterEach(() => vi.useRealTimers())

describe('v2 receiver content activation policy', () => {
  it('does no content-route or peer work while the receiver only browses', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)
    expect(offers.calls).toBe(0)
    expect(vi.getTimerCount()).toBe(0)
    expect(lanes.size).toBe(0)
    await connectivity.close()
  })

  it('keeps relay usable without allocating a binding when the native API is absent', async () => {
    vi.useFakeTimers()
    const offers = new SuccessfulOffers()
    const diagnostics: V2PeerAttemptTraceEvent[] = []
    let randomCalls = 0
    const { connectivity, lanes } = fixture(offers, undefined, {
      nativePeerUsable: () => false,
      randomBytes: (length) => {
        randomCalls += 1
        return new Uint8Array(length).fill(8)
      },
      attemptTraceObserver: (event) => diagnostics.push(event),
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

  it('contains API-gate exceptions without starting a task rejection', async () => {
    vi.useFakeTimers()
    const offers = new SuccessfulOffers()
    const predicateFailure = new Error('synthetic API gate failure')
    const { connectivity, lanes } = fixture(offers, undefined, {
      nativePeerUsable: () => { throw predicateFailure },
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(offers.calls).toBe(0)
    expect(lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)

    preview.close()
    await expect(connectivity.close()).resolves.toBeUndefined()
  })

  it('emits one schema-valid lifecycle and does not fail it during normal cleanup', async () => {
    vi.useFakeTimers()
    const diagnostics: V2PeerAttemptTraceEvent[] = []
    const { connectivity, lanes } = fixture(new SuccessfulOffers(), undefined, {
      attemptTraceObserver: (event) => diagnostics.push(event),
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(lanes.laneIds()).toEqual([1, 2])
    expect(diagnostics.map((event) => event.stage)).toEqual([
      'started',
      'negotiation-deadline-armed',
      'offer-created',
      'offer-sent',
      'answer-received',
      'datachannel-open',
      'admission-deadline-armed',
      'grant-requested',
      'grant-received',
      'lane-hello-sent',
      'admission-response-received',
      'admission-response-settled',
      'lane-attached',
      'admitted',
    ])
    expect(diagnostics.every((event) =>
      event.correlation.protocolSessionId?.kind === 'protocol_session' &&
      event.correlation.peerPathId?.kind === 'peer_path' &&
      event.correlation.peerAttemptId?.kind === 'peer_attempt'
    )).toBe(true)
    expect(diagnostics.at(-1)).toMatchObject({
      stage: 'admitted',
      correlation: { lane: { id: 2, epoch: 1 } },
    })

    preview.close()
    await connectivity.close()
    expect(diagnostics.filter((event) => event.stage === 'failed')).toEqual([])
  })

  it('isolates observer exceptions from authenticated lane admission', async () => {
    vi.useFakeTimers()
    const stages: string[] = []
    const { connectivity, lanes, errors } = fixture(new SuccessfulOffers(), undefined, {
      attemptTraceObserver: (event) => {
        stages.push(event.stage)
        throw new Error('synthetic diagnostic observer failure')
      },
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(stages.at(-1)).toBe('admitted')
    expect(lanes.laneIds()).toEqual([1, 2])
    expect(errors).toEqual([])

    preview.close()
    await connectivity.close()
  })

  it('blocks grant, attach, and admission after a post-Open authenticated failure', async () => {
    vi.useFakeTimers()
    const diagnostics: V2PeerAttemptTraceEvent[] = []
    const { connectivity, lanes, session } = fixture(new SuccessfulOffers(), undefined, {
      attemptTraceObserver: (event) => diagnostics.push(event),
    })
    session.grantGate = new Promise<void>(() => undefined)
    const preview = connectivity.begin('preview')
    await turn()
    expect(session.grantCalls).toBe(1)

    session.failPeerOperation(authenticatedPeerFailure(0x5004))
    await turn()

    expect(diagnostics.map((event) => event.stage)).toEqual([
      'started',
      'negotiation-deadline-armed',
      'offer-created',
      'offer-sent',
      'answer-received',
      'datachannel-open',
      'admission-deadline-armed',
      'failed',
    ])
    expect(diagnostics.at(-1)).toMatchObject({
      failedAtStage: 'grant-requested',
      typedErrorCode: 'peer-admission',
      failure: {
        kind: 'authenticated-peer-operation',
        code: 0x5004,
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
    const diagnostics: V2PeerAttemptTraceEvent[] = []
    const { connectivity, lanes } = fixture({
      offer: async () => { throw new Error('independent probe did not control this attempt') },
    }, undefined, {
      attemptTraceObserver: (event) => diagnostics.push(event),
    })

    const preview = connectivity.begin('preview')
    await turn()

    expect(diagnostics.map((event) => event.stage)).toEqual([
      'started',
      'negotiation-deadline-armed',
      'failed',
    ])
    expect(diagnostics.at(-1)).toMatchObject({
      failedAtStage: 'offer-created',
      failureScope: 'attempt',
      typedErrorCode: 'signaling-contract',
    })
    expect(lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)

    preview.close()
    await connectivity.close()
    expect(diagnostics.filter((event) => event.stage === 'failed')).toHaveLength(1)
  })

  it('cancels a pending attempt without manufacturing failure authority', async () => {
    vi.useFakeTimers()
    const diagnostics: V2PeerAttemptTraceEvent[] = []
    const offers = new PendingOffers()
    const { connectivity } = fixture(offers, undefined, {
      attemptTraceObserver: (event) => diagnostics.push(event),
    })
    connectivity.begin('preview')
    await turn()

    await connectivity.close()

    expect(offers.aborts).toBe(1)
    expect(diagnostics.map((event) => event.stage)).toEqual([
      'started',
      'negotiation-deadline-armed',
    ])
    expect(diagnostics.filter((event) => event.stage === 'failed')).toEqual([])
  })
})

describe('v2 receiver immediate dual-path policy', () => {
  it('admits relay synchronously before a pending peer offer can settle', async () => {
    vi.useFakeTimers()
    const order: string[] = []
    const offers = new PendingOffers(() => order.push('peer-offer'))
    const { connectivity, lanes } = fixture(
      offers,
      (observation) => order.push(`${observation.route}-admitted`),
    )

    const preview = connectivity.begin('preview')

    expect(order).toEqual(['relay-admitted'])
    await turn()
    expect(order).toEqual(['relay-admitted', 'peer-offer'])
    expect(offers.calls).toBe(1)
    expect(lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)
    expect(preview.routes.allows('peer')).toBe(true)
    expect(vi.getTimerCount()).toBe(2)

    preview.close()
    expect(preview.routes.active).toBe(false)
    await connectivity.close()
  })

  it('gives downloads the same immediate relay and peer authority without size classification', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)

    const download = connectivity.begin('download')

    await turn()
    expect(offers.calls).toBe(1)
    expect(lanes.laneIds()).toEqual([1])
    expect(download.routes.allows('relay')).toBe(true)
    expect(download.routes.allows('peer')).toBe(true)

    download.close()
    await connectivity.close()
  })

  it('keeps the admitted relay lane usable throughout peer lane grant and attach', async () => {
    vi.useFakeTimers()
    const offers = new DeferredSuccessfulOffers()
    const { connectivity, lanes, session } = fixture(offers)
    const attach = deferred<void>()
    session.attachGate = attach.promise

    const download = connectivity.begin('download')
    await turn()
    expect(offers.calls).toBe(1)
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

    const download = result.connectivity.begin('download')
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

  it('keeps overlapping activation authority independent while sharing one peer attempt', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity, lanes } = fixture(offers)
    const preview = connectivity.begin('preview')
    const download = connectivity.begin('download')

    await turn()
    expect(offers.calls).toBe(1)
    expect(lanes.eligibleSize(preview.routes)).toBe(1)
    expect(lanes.eligibleSize(download.routes)).toBe(1)

    preview.close()
    expect(preview.routes.active).toBe(false)
    expect(() => lanes.eligibleSize(preview.routes)).toThrow('Content activation closed')
    expect(lanes.eligibleSize(download.routes)).toBe(1)
    expect(offers.aborts).toBe(0)

    download.close()
    await turn()
    expect(offers.aborts).toBe(1)
    expect(lanes.laneIds()).toEqual([1])
    await connectivity.close()
  })

  it('starts a replacement P2P attempt when a new click races old-attempt cleanup', async () => {
    vi.useFakeTimers()
    const offers = new PendingOffers()
    const { connectivity } = fixture(offers)
    const first = connectivity.begin('preview')
    await turn()
    first.close()
    const replacement = connectivity.begin('preview')
    expect(offers.calls).toBe(1)
    expect(replacement.routes.allows('relay')).toBe(true)
    await turn()
    expect(offers.calls).toBe(2)
    replacement.close()
    await connectivity.close()
  })

  it('keeps relay admitted through an explicit P2P failure', async () => {
    vi.useFakeTimers()
    const pending = fixture(new PendingOffers())
    const download = pending.connectivity.begin('download')
    expect(pending.lanes.laneIds()).toEqual([1])
    expect(download.routes.allows('relay')).toBe(true)
    download.close()
    await pending.connectivity.close()

    const failures = fixture({
      offer: async () => { throw new Error('P2P unavailable') },
    })
    const preview = failures.connectivity.begin('preview')
    await turn()
    expect(failures.lanes.laneIds()).toEqual([1])
    expect(preview.routes.allows('relay')).toBe(true)
    expect(failures.errors).toEqual([])
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
    expect(lanes.laneIds()).toEqual([1, 2])
    // Lane publication precedes recovery-state settlement by one async handoff.
    await turn()

    session.detach(2)
    await waitForMicrotaskCondition(
      'detachment replacement offer',
      () => offers.calls === 2,
    )
    await waitForMicrotaskCondition(
      'detachment replacement grant',
      () => session.requestedLaneIds.length === 2,
    )
    await waitForMicrotaskCondition(
      'detached logical lane to be readopted',
      () => lanes.laneIds().includes(2),
    )
    expect(detached).toEqual([{ laneId: 2, laneEpoch: 1, route: 'peer' }])
    expect(visibleLaneIds).toEqual([[1]])
    expect(lanes.laneIds()).toEqual([1, 2])
    expect(preview.routes.allows('relay')).toBe(true)
    expect(offers.calls).toBe(2)
    expect(session.requestedLaneIds).toEqual([0, 2])
    preview.close()
    await connectivity.close()
  })

  it('joins detached peer cleanup during shutdown while relay remains usable', async () => {
    vi.useFakeTimers()
    const offers = new GatedDetachedPeerCleanupOffers()
    const { connectivity, lanes, session } = fixture(offers)
    const preview = connectivity.begin('preview')
    await turn()
    expect(lanes.laneIds()).toEqual([1, 2])
    await turn()

    session.detach(2)
    await waitForMicrotaskCondition(
      'detached peer cleanup and supervisor recovery',
      () => offers.peerCloseCalls === 1 && offers.routeCloseCalls === 1 && offers.calls === 2,
    )
    session.detach(2)
    await turn()
    expect(offers.peerCloseCalls).toBe(1)
    expect(offers.routeCloseCalls).toBe(1)

    expect(lanes.laneIds()).toEqual([1])
    await expect(lanes.fetch(
      relayDemand(),
      preview.routes,
      new AbortController().signal,
    )).resolves.toMatchObject({ data: Uint8Array.of(1) })

    let shutdownSettled = false
    const shutdown = connectivity.close().then(() => {
      shutdownSettled = true
    })
    await turn()
    expect(shutdownSettled).toBe(false)
    expect(offers.peerCloseCalls).toBe(1)
    expect(offers.routeCloseCalls).toBe(1)

    offers.releasePeerClose()
    await turn()
    expect(shutdownSettled).toBe(false)

    offers.releaseRouteClose()
    await shutdown
    expect(shutdownSettled).toBe(true)
    expect(offers.peerCloseCalls).toBe(1)
    expect(offers.routeCloseCalls).toBe(1)
  })

  it('keeps relay and the ProtocolSession usable through wave and session exhaustion', async () => {
    const clock = new ManualRecoveryClock()
    const events: V2PeerRecoveryTraceEvent[] = []
    let attempts = 0
    const result = fixture({
      offer: async () => {
        attempts += 1
        throw new PeerNegotiationError('synthetic transient transport loss')
      },
    }, undefined, {
      recoveryTraceObserver: (event) => events.push(event),
      peerRecovery: {
        clock,
        random: () => 1,
        policy: {
          negotiationBudgetMilliseconds: 100,
          admissionBudgetMilliseconds: 100,
          waveMaxAttempts: 2,
          waveElapsedBudgetMilliseconds: 10_000,
          sessionMaxAttempts: 3,
          sessionActiveElapsedBudgetMilliseconds: 20_000,
          retryInitialBackoffMilliseconds: 1,
          retryBackoffMultiplier: 1,
          retryBackoffMaximumMilliseconds: 1,
          retryJitterMinimumFactor: 1,
          retryJitterMaximumFactor: 1,
        },
      },
    })

    const first = result.connectivity.begin('preview')
    await turn()
    await clock.advance(1)
    expect(attempts).toBe(2)
    expect(events).toContainEqual(expect.objectContaining({
      stage: 'wave-quiesced',
      reason: 'wave-attempt-budget',
    }))

    const rearm = result.connectivity.begin('download')
    await turn()
    expect(attempts).toBe(3)
    expect(events).toContainEqual(expect.objectContaining({
      stage: 'session-budget-exhausted',
      reason: 'session-attempt-budget',
    }))
    expect(result.session.closeCalls).toBe(0)
    expect(result.lanes.laneIds()).toEqual([1])
    await expect(result.lanes.fetch(
      relayDemand(),
      first.routes,
      new AbortController().signal,
    )).resolves.toMatchObject({ data: Uint8Array.of(1) })

    rearm.close()
    first.close()
    await result.connectivity.close()
  })
})
