import { V2_RELAY_ERROR } from '../../src/transport/relay/v2-protocol'
import { V2RelayReceiverError } from '../../src/transport/relay/v2-receiver'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import { FileGeometry } from '../../src/content/geometry'
import type {
  V2BlockDemand,
  V2BlockDispatchObservation,
  V2BlockLane,
  V2BlockRouteEligibility,
} from '../../src/content/v2-broker'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
import type { Suite02CapabilityKey } from '../../src/crypto/suite02-link'
import {
  V2ConnectivityRouteAuthority,
  type V2ContentLaneAdmissionObservation,
} from '../../src/connectivity/v2-receiver-policy'
import type { V2PeerAttemptTraceEvent } from '../../src/connectivity/diagnostics'
import {
  V2BrowserSessionFactory,
  type V2AttachedRelay,
  type V2ProtocolGenerationCore,
  type V2ReceiverSessionFactory,
  V2StaleShareInstanceError,
} from '../../src/receiver/v2-session-factory'
import { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import {
  createV2ProtocolSessionIdentity,
  type V2ProtocolSessionIdentity,
} from '../../src/session/v2-identities'
import { V2SessionRuntimeError, type V2LaneChange } from '../../src/session/v2-runtime-types'
import type { V2RelayReceiverConnection } from '../../src/transport/relay/v2-receiver'

const ALL_BLOCK_ROUTES: V2BlockRouteEligibility = Object.freeze({
  active: true,
  allows: () => true,
  assertActive: () => undefined,
  subscribe: () => () => undefined,
})

afterEach(() => vi.useRealTimers())

class FakeSession {
  subscribePeerPathControls(): () => void { return () => undefined }
  async sendPeerPathControl(): Promise<void> {}
  readonly initialLaneId: number
  readonly keys: { readonly protocolSessionId: Uint8Array<ArrayBuffer>; readonly initialLaneEpoch: 0 }
  readonly protocolSessionIdentity: V2ProtocolSessionIdentity
  readonly #laneIds: Set<number>
  readonly #listeners = new Set<(change: V2LaneChange) => void>()
  closeCalls = 0
  isClosed = false

  constructor(laneIds: readonly number[]) {
    if (laneIds.length === 0) throw new Error('A fake generation needs an initial lane')
    this.initialLaneId = laneIds[0]!
    this.keys = Object.freeze({
      protocolSessionId: identity(this.initialLaneId + 100),
      initialLaneEpoch: 0,
    })
    this.protocolSessionIdentity = createV2ProtocolSessionIdentity(this.keys.protocolSessionId)
    this.#laneIds = new Set(laneIds)
  }

  laneIds(): readonly number[] {
    return [...this.#laneIds]
  }

  subscribeLaneChanges(listener: (change: V2LaneChange) => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  attach(laneId: number): void {
    this.#laneIds.add(laneId)
    this.#emit({ type: 'attached', laneId, laneEpoch: 1 })
  }

  detach(laneId: number, failure?: unknown): void {
    this.#laneIds.delete(laneId)
    this.#emit({
      type: 'detached',
      laneId,
      laneEpoch: 1,
      ...(failure === undefined ? {} : { failure }),
    })
  }

  async close(): Promise<void> {
    this.closeCalls += 1
    this.isClosed = true
  }

  #emit(change: V2LaneChange): void {
    for (const listener of this.#listeners) listener(change)
  }
}

class TrackedRelay {
  closeCalls = 0
  readonly connection: V2RelayReceiverConnection

  constructor(id: number) {
    this.connection = {
      endpoint: {} as V2RelayReceiverConnection['endpoint'],
      relaySessionId: identity(id),
      descriptorObject: Uint8Array.of(id),
      channel: {} as V2RelayReceiverConnection['channel'],
      close: async () => { this.closeCalls += 1 },
    }
  }
}

class FakeSessionFactory implements V2ReceiverSessionFactory {
  readonly relayBases = ['https://relay.example']
  attachRelayCalls = 0
  connectFreshCalls = 0
  closeCalls = 0
  attachRelayImpl: (
    session: V2ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ) => Promise<V2AttachedRelay> = async () => {
    throw new Error('Unexpected relay attachment')
  }
  connectFreshImpl: (signal: AbortSignal) => Promise<V2ProtocolGenerationCore> = async () => {
    throw new Error('Unexpected fresh generation')
  }
  #closed = false

  attachRelay(
    session: V2ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ): Promise<V2AttachedRelay> {
    this.attachRelayCalls += 1
    return this.attachRelayImpl(session, signal, relayBase)
  }

  connectFresh(signal: AbortSignal): Promise<V2ProtocolGenerationCore> {
    this.connectFreshCalls += 1
    return this.connectFreshImpl(signal)
  }

  copyReadSecret(): Uint8Array<ArrayBuffer> {
    return new Uint8Array(32).fill(7)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.closeCalls += 1
  }
}

interface Deferred<T> {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accepted) => { resolve = accepted })
  return { promise, resolve }
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

const blockDescriptor: V2FileRevisionDescriptor = Object.freeze({
  shareInstance: identity(70),
  shareInstanceId: 'dispatch-share',
  fileId: identity(71),
  fileIdText: 'dispatch-file',
  fileRevision: identity(72),
  fileRevisionText: 'dispatch-revision',
  exactSize: 2n,
  geometry: new FileGeometry(2n, 1n),
})

function blockDemand(localBlockIndex: bigint): V2BlockDemand {
  return { descriptor: blockDescriptor, leaseId: identity(73), localBlockIndex }
}

class ImmediateBlockLane implements V2BlockLane {
  readonly id: number

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(demand: V2BlockDemand): Promise<V2BlockRecord> {
    return Promise.resolve({
      descriptor: blockDescriptor,
      localBlockIndex: demand.localBlockIndex,
      data: Uint8Array.of(this.id),
    })
  }
}

function descriptor(seed = 1): V2ShareDescriptor {
  return Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance: identity(seed),
    shareInstanceId: `share-${seed}`,
    syntheticRoot: identity(seed + 1),
    syntheticRootId: `root-${seed}`,
    chunkSize: 65_536,
    capabilities: 0n,
    senderPublicKey: new Uint8Array(32).fill(seed + 2),
    createdAtSeconds: BigInt(seed),
    pathPolicy: V2_PATH_POLICY,
  })
}

function core(
  session: FakeSession,
  relay: TrackedRelay,
  relayLaneId = session.initialLaneId,
): V2ProtocolGenerationCore {
  return {
    relayBase: 'https://relay.example',
    session: session as unknown as V2ReceiverSessionRuntime,
    relay: relay.connection,
    relayLaneId,
  }
}

function supervisorFixture(
  session: FakeSession,
  relay: TrackedRelay,
  factory = new FakeSessionFactory(),
  share = descriptor(),
  onContentLaneAdmitted?: (observation: V2ContentLaneAdmissionObservation) => void,
) {
  const supervisor = new V2ReceiverReconnectSupervisor({
    descriptor: share,
    initial: core(session, relay),
    sessionFactory: factory,
    randomBytes: (length) => new Uint8Array(length).fill(9),
    ...(onContentLaneAdmitted === undefined ? {} : { onContentLaneAdmitted }),
  })
  return { factory, supervisor }
}

async function dispatchOnCurrentGeneration(
  supervisor: V2ReceiverReconnectSupervisor,
  laneId: number,
  localBlockIndex: bigint,
): Promise<void> {
  await supervisor.execute(undefined, async (generation) => {
    for (const currentLaneId of generation.lanes.laneIds()) generation.lanes.remove(currentLaneId)
    generation.lanes.add(new ImmediateBlockLane(laneId), 'application-relay', 0)
    await generation.lanes.fetch(
      blockDemand(localBlockIndex),
      ALL_BLOCK_ROUTES,
      new AbortController().signal,
    )
  })
}

async function flushReconciliation(): Promise<void> {
  for (let turn = 0; turn < 16; turn += 1) await Promise.resolve()
}

it('keeps download identity across generation changes and releases its bounded ledger on close', async () => {
  const initial = new FakeSession([1])
  const { supervisor } = supervisorFixture(initial, new TrackedRelay(1))
  supervisor.pathActivity.admitted(supervisor.generationId, { laneId: 9, laneEpoch: 2, route: 'direct' })
  const activation = supervisor.beginConnectivity('download')
  const metrics = supervisor.downloadMetrics(activation.routes)!
  expect(metrics.snapshot().first_direct_elapsed_ms).toBe(0)
  const downloadId = metrics.snapshot().download_id
  supervisor.pathActivity.detached(supervisor.generationId, { laneId: 9, laneEpoch: 1, route: 'direct' })
  expect(metrics.snapshot().first_direct_elapsed_ms).toBe(0)
  supervisor.pathActivity.generationInstalled(supervisor.generationId + 1)
  expect(supervisor.downloadMetrics(activation.routes)?.snapshot().download_id).toBe(downloadId)
  activation.close()
  expect(metrics.snapshot().final).toBe(true)
  expect(supervisor.downloadMetrics(activation.routes)).toBeUndefined()
  await supervisor.close()
})

describe('v2 receiver reconnect supervisor', () => {
  it('dispatches relay content while the peer offer remains pending', async () => {
    const session = new FakeSession([1])
    const relay = new TrackedRelay(1)
    const factory = new FakeSessionFactory()
    const dispatches: V2BlockDispatchObservation[] = []
    let offerCalls = 0
    let offerAborts = 0
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(),
      initial: core(session, relay),
      sessionFactory: factory,
      randomBytes: (length) => new Uint8Array(length).fill(11),
      offersFactory: () => ({
        offer: (_route, signal) => {
          offerCalls += 1
          return new Promise((_resolve, reject) => {
            const abort = () => {
              offerAborts += 1
              reject(signal.reason)
            }
            signal.addEventListener('abort', abort, { once: true })
            if (signal.aborted) abort()
          })
        },
      }),
      nativePeerUsable: () => true,
      onBlockDispatched: (observation) => dispatches.push(observation),
    })

    const activation = supervisor.beginConnectivity('download')

    expect(supervisor.contentLaneCount(activation.routes)).toBe(1)
    await flushReconciliation()
    expect(offerCalls).toBe(1)
    await supervisor.execute(undefined, async (generation) => {
      expect(generation.lanes.laneIds()).toEqual([1])
      generation.lanes.remove(1)
      generation.lanes.add(new ImmediateBlockLane(1), 'application-relay', 0)
      const block = await generation.lanes.fetch(
        blockDemand(0n),
        activation.routes,
        new AbortController().signal,
      )
      expect(block.data).toEqual(Uint8Array.of(1))
    })
    expect(dispatches).toMatchObject([{ route: 'application-relay', laneId: 1 }])

    activation.close()
    await flushReconciliation()
    expect(offerAborts).toBe(0)
    expect(supervisor.generationId).toBe(1)
    expect(factory.connectFreshCalls).toBe(0)
    await supervisor.close()
  })

  it('projects connectivity attempt diagnostics through the generation boundary', async () => {
    const session = new FakeSession([1])
    const relay = new TrackedRelay(1)
    const factory = new FakeSessionFactory()
    const diagnostics: V2PeerAttemptTraceEvent[] = []
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(),
      initial: core(session, relay),
      sessionFactory: factory,
      randomBytes: (length) => new Uint8Array(length).fill(12),
      offersFactory: () => ({
        offer: async () => { throw new Error('synthetic observed peer failure') },
      }),
      nativePeerUsable: () => true,
      connectivityTrace: { current: (event) => {
        if (event.eventName === 'peer_attempt') diagnostics.push(event)
      } },
    })

    const activation = supervisor.beginConnectivity('preview')
    await flushReconciliation()

    expect(diagnostics.filter((event) => event.stage !== 'provider-fact').map((event) => event.stage)).toEqual([
      'started',
      'negotiation-deadline-armed',
      'failed',
    ])
    expect(diagnostics.at(-1)).toMatchObject({
      failedAtStage: 'offer-created',
      typedErrorCode: 'signaling-contract',
    })

    activation.close()
    await supervisor.close()
  })

  it('preserves lane-admission observations across generation replacement', async () => {
    const firstSession = new FakeSession([1])
    const firstRelay = new TrackedRelay(1)
    const secondSession = new FakeSession([10])
    const secondRelay = new TrackedRelay(10)
    const factory = new FakeSessionFactory()
    factory.connectFreshImpl = async () => core(secondSession, secondRelay)
    const observations: V2ContentLaneAdmissionObservation[] = []
    const { supervisor } = supervisorFixture(
      firstSession,
      firstRelay,
      factory,
      descriptor(),
      (observation) => observations.push(observation),
    )
    const download = supervisor.connectivity.begin('download')

    expect(observations).toEqual([{ laneId: 1, laneEpoch: 0, route: 'application-relay' }])
    firstSession.detach(1)
    await flushReconciliation()

    expect(supervisor.generationId).toBe(2)
    expect(supervisor.protocolSessionIdentity.copyBytes()).toEqual(
      secondSession.protocolSessionIdentity.copyBytes(),
    )
    expect(observations).toEqual([
      { laneId: 1, laneEpoch: 0, route: 'application-relay' },
      { laneId: 10, laneEpoch: 0, route: 'application-relay' },
    ])

    download.close()
    await supervisor.close()
  })

  it('allocates fresh peer recovery identities and budgets for a replacement generation', async () => {
    const firstSession = new FakeSession([1])
    const firstRelay = new TrackedRelay(1)
    const secondSession = new FakeSession([10])
    const secondRelay = new TrackedRelay(10)
    const factory = new FakeSessionFactory()
    factory.connectFreshImpl = async () => core(secondSession, secondRelay)
    const milestones: V2PeerAttemptTraceEvent[] = []
    let seed = 20
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(),
      initial: core(firstSession, firstRelay),
      sessionFactory: factory,
      randomBytes: (length) => new Uint8Array(length).fill(seed++),
      offersFactory: () => ({
        offer: async () => { throw new Error('synthetic path-scoped failure') },
      }),
      nativePeerUsable: () => true,
      connectivityTrace: { current: (event) => {
        if (event.eventName === 'peer_attempt') milestones.push(event)
      } },
    })
    const activation = supervisor.beginConnectivity('preview')
    await flushReconciliation()
    const first = milestones.find((milestone) =>
      milestone.stage === 'negotiation-deadline-armed')
    expect(first).toBeDefined()

    firstSession.detach(1)
    await flushReconciliation()
    const armed = milestones.filter((milestone) =>
      milestone.stage === 'negotiation-deadline-armed')
    expect(armed).toHaveLength(2)
    expect(armed[1]?.correlation.protocolSessionId?.copyBytes()).toEqual(
      secondSession.protocolSessionIdentity.copyBytes(),
    )
    expect(armed[1]?.correlation.protocolSessionId?.copyBytes()).not.toEqual(
      armed[0]?.correlation.protocolSessionId?.copyBytes(),
    )
    expect(armed[1]?.correlation.peerPathId?.copyBytes()).not.toEqual(
      armed[0]?.correlation.peerPathId?.copyBytes(),
    )
    expect(armed[1]?.correlation.peerAttemptId?.copyBytes()).not.toEqual(
      armed[0]?.correlation.peerAttemptId?.copyBytes(),
    )
    expect(supervisor.contentLaneCount(activation.routes)).toBe(1)

    activation.close()
    await supervisor.close()
  })

  it('keeps dispatch observations contiguous across fresh protocol generations', async () => {
    const firstSession = new FakeSession([1])
    const firstRelay = new TrackedRelay(1)
    const secondSession = new FakeSession([10])
    const secondRelay = new TrackedRelay(10)
    const factory = new FakeSessionFactory()
    factory.connectFreshImpl = async () => core(secondSession, secondRelay)
    const dispatches: V2BlockDispatchObservation[] = []
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(),
      initial: core(firstSession, firstRelay),
      sessionFactory: factory,
      randomBytes: (length) => new Uint8Array(length).fill(15),
      onBlockDispatched: (observation) => dispatches.push(observation),
    })
    const download = supervisor.beginConnectivity('download')

    await dispatchOnCurrentGeneration(supervisor, 1, 0n)
    firstSession.detach(1)
    await flushReconciliation()
    expect(supervisor.generationId).toBe(2)
    await dispatchOnCurrentGeneration(supervisor, 10, 1n)

    expect(dispatches.map((observation) => observation.dispatchSequence)).toEqual([1, 2])
    download.close()
    await supervisor.close()
  })

  it('reattaches one relay to the same generation when P2P survives', async () => {
    const session = new FakeSession([1, 2])
    const initialRelay = new TrackedRelay(1)
    const replacementRelay = new TrackedRelay(3)
    const factory = new FakeSessionFactory()
    factory.attachRelayImpl = async (target, signal) => {
      signal.throwIfAborted()
      expect(target).toBe(session as unknown as V2ReceiverSessionRuntime)
      session.attach(3)
      return { relay: replacementRelay.connection, laneId: 3 }
    }
    const { supervisor } = supervisorFixture(session, initialRelay, factory)
    const generations: number[] = []
    supervisor.subscribeProtocolGeneration(observation => {
      generations.push(observation.generationId)
    })

    session.detach(1)
    await flushReconciliation()

    expect(factory.attachRelayCalls).toBe(1)
    expect(factory.connectFreshCalls).toBe(0)
    expect(supervisor.generationId).toBe(1)
    expect(session.laneIds()).toEqual([2, 3])
    expect(initialRelay.closeCalls).toBe(1)
    expect(generations).toEqual([])

    await supervisor.close()
  })

  it('singleflights a fresh generation when every lane is lost', async () => {
    const firstSession = new FakeSession([1])
    const firstRelay = new TrackedRelay(1)
    const secondSession = new FakeSession([10])
    const secondRelay = new TrackedRelay(10)
    const fresh = deferred<V2ProtocolGenerationCore>()
    const factory = new FakeSessionFactory()
    factory.connectFreshImpl = () => fresh.promise
    const { supervisor } = supervisorFixture(firstSession, firstRelay, factory)
    const generations: Array<{ generationId: number; protocolSessionIdentity: unknown }> = []
    supervisor.subscribeProtocolGeneration(observation => {
      generations.push({
        generationId: observation.generationId,
        protocolSessionIdentity: observation.protocolSessionIdentity,
      })
    })
    const waiters = [
      supervisor.waitForGenerationAfter(1),
      supervisor.waitForGenerationAfter(1),
      supervisor.waitForGenerationAfter(1),
    ]

    firstSession.detach(1)
    firstSession.detach(1)
    await flushReconciliation()
    expect(factory.connectFreshCalls).toBe(1)
    expect(supervisor.generationId).toBe(1)

    fresh.resolve(core(secondSession, secondRelay))
    await Promise.all(waiters)

    expect(factory.connectFreshCalls).toBe(1)
    expect(factory.attachRelayCalls).toBe(0)
    expect(supervisor.generationId).toBe(2)
    expect(firstSession.closeCalls).toBeGreaterThan(0)
    expect(generations).toEqual([{
      generationId: 2,
      protocolSessionIdentity: secondSession.protocolSessionIdentity,
    }])

    await supervisor.close()
  })

  registerProtocolSessionReplacementWaitTests()

  it('publishes stop before lane cleanup can request another dial', async () => {
    const session = new FakeSession([1, 2])
    const relay = new TrackedRelay(1)
    const { factory, supervisor } = supervisorFixture(session, relay)

    await supervisor.close()
    session.detach(1)
    await flushReconciliation()

    expect(supervisor.isStopped).toBe(true)
    expect(factory.attachRelayCalls).toBe(0)
    expect(factory.connectFreshCalls).toBe(0)
    expect(factory.closeCalls).toBe(1)
  })

  it('treats a changed ShareInstance during relay recovery as terminal', async () => {
    const session = new FakeSession([1, 2])
    const relay = new TrackedRelay(1)
    const factory = new FakeSessionFactory()
    const stale = new V2StaleShareInstanceError('stale authenticated share')
    factory.attachRelayImpl = async () => { throw stale }
    const { supervisor } = supervisorFixture(session, relay, factory)
    const terminal = expect(supervisor.waitForGenerationAfter(1)).rejects.toBe(stale)

    session.detach(1)
    await terminal

    expect(factory.attachRelayCalls).toBe(1)
    expect(factory.connectFreshCalls).toBe(0)
    expect(factory.closeCalls).toBe(1)
    expect(supervisor.contentLaneCount(new V2ConnectivityRouteAuthority())).toBe(0)

    await supervisor.close()
  })

  it('does not redial after an authenticated session failure while another lane survives', async () => {
    const session = new FakeSession([1, 2])
    const relay = new TrackedRelay(1)
    const { factory, supervisor } = supervisorFixture(session, relay)
    const fatal = new V2SessionRuntimeError('session', 'authenticated envelope failed')
    const terminal = expect(supervisor.waitForGenerationAfter(1)).rejects.toBe(fatal)

    session.detach(1, fatal)
    await terminal
    await flushReconciliation()

    expect(factory.attachRelayCalls).toBe(0)
    expect(factory.connectFreshCalls).toBe(0)
    expect(factory.closeCalls).toBe(1)

    await supervisor.close()
  })

  it('keeps reconnect authority isolated between receivers', async () => {
    const firstSession = new FakeSession([1, 2])
    const secondSession = new FakeSession([1, 2])
    const firstRelay = new TrackedRelay(1)
    const secondRelay = new TrackedRelay(2)
    const firstFactory = new FakeSessionFactory()
    const secondFactory = new FakeSessionFactory()
    firstFactory.attachRelayImpl = async () => {
      firstSession.attach(3)
      return { relay: new TrackedRelay(3).connection, laneId: 3 }
    }
    const first = supervisorFixture(firstSession, firstRelay, firstFactory, descriptor(1)).supervisor
    const second = supervisorFixture(secondSession, secondRelay, secondFactory, descriptor(20)).supervisor

    firstSession.detach(1)
    await flushReconciliation()

    expect(firstFactory.attachRelayCalls).toBe(1)
    expect(secondFactory.attachRelayCalls).toBe(0)
    expect(first.generationId).toBe(1)
    expect(second.generationId).toBe(1)
    expect(secondSession.laneIds()).toEqual([1, 2])

    await Promise.all([first.close(), second.close()])
  })
})

function registerProtocolSessionReplacementWaitTests(): void {
  it('waits by issuing ProtocolSession identity and resolves immediately when already stale', async () => {
    const firstSession = new FakeSession([1])
    const firstRelay = new TrackedRelay(1)
    const secondSession = new FakeSession([10])
    const secondRelay = new TrackedRelay(10)
    const fresh = deferred<V2ProtocolGenerationCore>()
    const factory = new FakeSessionFactory()
    factory.connectFreshImpl = () => fresh.promise
    const { supervisor } = supervisorFixture(firstSession, firstRelay, factory)
    const replacement = supervisor.waitForProtocolSessionReplacement(
      firstSession.protocolSessionIdentity,
      new AbortController().signal,
    )

    firstSession.detach(1)
    await flushReconciliation()
    fresh.resolve(core(secondSession, secondRelay))
    await replacement
    await expect(supervisor.waitForProtocolSessionReplacement(
      firstSession.protocolSessionIdentity,
      new AbortController().signal,
    )).resolves.toBeUndefined()

    const controller = new AbortController()
    const reason = new DOMException('capacity wait canceled', 'AbortError')
    const canceled = supervisor.waitForProtocolSessionReplacement(
      secondSession.protocolSessionIdentity,
      controller.signal,
    )
    controller.abort(reason)
    await expect(canceled).rejects.toBe(reason)
    await supervisor.close()
  })

  it('does not treat same-generation relay reattachment as ProtocolSession replacement', async () => {
    const session = new FakeSession([1, 2])
    const initialRelay = new TrackedRelay(1)
    const replacementRelay = new TrackedRelay(3)
    const factory = new FakeSessionFactory()
    factory.attachRelayImpl = async () => {
      session.attach(3)
      return { relay: replacementRelay.connection, laneId: 3 }
    }
    const { supervisor } = supervisorFixture(session, initialRelay, factory)
    const controller = new AbortController()
    let replaced = false
    const waiting = supervisor.waitForProtocolSessionReplacement(
      session.protocolSessionIdentity,
      controller.signal,
    ).then(() => { replaced = true })

    session.detach(1)
    await flushReconciliation()
    expect(supervisor.generationId).toBe(1)
    expect(replaced).toBe(false)
    controller.abort(new DOMException('test completed', 'AbortError'))
    await expect(waiting).rejects.toMatchObject({ name: 'AbortError' })
    await supervisor.close()
  })
}

describe('bounded protocol generation replacement', () => {
  it('retires failed authority and stops after the recovery wave', async () => {
    const session = new FakeSession([1])
    const relay = new TrackedRelay(1)
    const factory = new FakeSessionFactory()
    factory.connectFreshImpl = async () => { throw new Error('network unavailable') }
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(), initial: core(session, relay), sessionFactory: factory,
      policy: 'relay-only', clock: { now: () => 0, sleep: async () => undefined },
    })
    const terminal = expect(supervisor.waitForGenerationAfter(1)).rejects.toThrow('within its budget')
    session.detach(1)
    await terminal
    expect(factory.connectFreshCalls).toBe(4)
    expect(supervisor.generationId).toBe(1)
    expect(session.isClosed).toBe(true)
    await supervisor.close()
  })
})

describe('independent receiver relay set', () => {
  it('admits a fast extra relay while another is pending and reconnects only the failed endpoint', async () => {
    const session = new FakeSession([1])
    const initial = new TrackedRelay(1)
    const second = new TrackedRelay(2)
    const third = new TrackedRelay(3)
    const replacement = new TrackedRelay(4)
    const slow = deferred<V2AttachedRelay>()
    const factory = new FakeSessionFactory()
    factory.relayBases.push('https://slow.example', 'https://fast.example')
    const requested: string[] = []
    factory.attachRelayImpl = async (_session, _signal, relayBase) => {
      requested.push(relayBase)
      if (relayBase === 'https://slow.example') return slow.promise
      const replacing = requested.filter((base) => base === relayBase).length > 1
      const laneId = replacing ? 4 : 3
      session.attach(laneId)
      return { relay: replacing ? replacement.connection : third.connection, laneId }
    }
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(), initial: core(session, initial), sessionFactory: factory, policy: 'relay-only',
    })
    const activation = supervisor.beginConnectivity('download')
    await flushReconciliation()
    expect(requested).toEqual(['https://slow.example', 'https://fast.example'])
    expect(supervisor.contentLaneCount(activation.routes)).toBe(2)
    expect(initial.closeCalls).toBe(0)
    session.attach(2)
    slow.resolve({ relay: second.connection, laneId: 2 })
    await flushReconciliation()
    expect(supervisor.contentLaneCount(activation.routes)).toBe(3)
    session.detach(3)
    await flushReconciliation()
    expect(supervisor.generationId).toBe(1)
    expect(requested).toEqual(['https://slow.example', 'https://fast.example', 'https://fast.example'])
    expect(supervisor.contentLaneCount(activation.routes)).toBe(3)
    expect(second.closeCalls).toBe(0)
    expect(third.closeCalls).toBe(1)
    activation.close()
    await supervisor.close()
    expect([initial.closeCalls, second.closeCalls, replacement.closeCalls]).toEqual([1, 1, 1])
  })

  it('never admits initial, extra, or reconnected relay content in p2p-only', async () => {
    const session = new FakeSession([1])
    const initial = new TrackedRelay(1)
    const extra = new TrackedRelay(2)
    const replaced = new TrackedRelay(3)
    const factory = new FakeSessionFactory()
    factory.relayBases.push('https://extra.example')
    let calls = 0
    factory.attachRelayImpl = async () => {
      const laneId = ++calls + 1
      session.attach(laneId)
      return { relay: calls === 1 ? extra.connection : replaced.connection, laneId }
    }
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(), initial: core(session, initial), sessionFactory: factory,
      policy: 'p2p-only', nativePeerUsable: () => false,
    })
    const activation = supervisor.beginConnectivity('download')
    await flushReconciliation()
    expect(supervisor.contentLaneCount(activation.routes)).toBe(0)
    session.detach(2)
    await flushReconciliation()
    expect(calls).toBe(2)
    expect(supervisor.contentLaneCount(activation.routes)).toBe(0)
    activation.close()
    await supervisor.close()
  })
})

describe('v2 browser session factory descriptor continuity', () => {
  it('retains endpoint failure identity and disables only STOP across fresh generations', async () => {
    const expected = descriptor(1)
    const relay = new TrackedRelay(4)
    const calls: string[] = []
    const stopped = new V2RelayReceiverError('stopped', { relayError: {
      code: V2_RELAY_ERROR.stopped, retryAfterMilliseconds: 0,
    } })
    const factory = new V2BrowserSessionFactory({
      relayBases: ['stopped', 'recoverable'], descriptor: expected,
      capability: { suite: 2, readSecret: identity(1), pkHash: identity(2),
        shareIdRaw: new Uint8Array(12), shareId: 'share' },
      descriptorObject: relay.connection.descriptorObject,
      dialRelay: async (endpoint) => {
        calls.push(endpoint)
        if (endpoint === 'stopped') throw stopped
        if (calls.filter((value) => value === 'recoverable').length === 1) throw new Error('outage')
        return relay.connection
      },
      openDescriptor: async () => expected,
      connectSession: async () => new FakeSession([4]) as unknown as V2ReceiverSessionRuntime,
    })
    await expect(factory.connectFresh(new AbortController().signal)).rejects.toMatchObject({
      errors: [{ relayBase: 'stopped', cause: stopped }, { relayBase: 'recoverable', cause: expect.any(Error) }],
    })
    const next = await factory.connectFresh(new AbortController().signal)
    expect(next.relayBase).toBe('recoverable')
    expect(calls).toEqual(['stopped', 'recoverable', 'recoverable'])
    await next.session.close()
    await next.relay.close()
    factory.close()
  })

  it('owns one named relay admission deadline outside the session runtime', async () => {
    vi.useFakeTimers()
    const expected = descriptor(1)
    const relay = new TrackedRelay(4)
    const capability: Suite02CapabilityKey = {
      suite: 2,
      readSecret: new Uint8Array(16).fill(1),
      pkHash: new Uint8Array(16).fill(2),
      shareIdRaw: new Uint8Array(12).fill(3),
      shareId: 'share',
    }
    const signals: AbortSignal[] = []
    const session = {
      requestLaneGrant: async (_laneId: number, options: { readonly signal: AbortSignal }) => {
        signals.push(options.signal)
        return {
          laneId: 9,
          laneEpoch: 1,
          grantOperationId: identity(91),
          attachNonce: identity(92),
        }
      },
      adoptGrantedLane: async (
        _channel: unknown,
        _grant: unknown,
        options: { readonly signal: AbortSignal },
      ) => {
        signals.push(options.signal)
        return Object.freeze({
          disposition: 'accepted' as const,
          installation: 'installed' as const,
          grantOperationId: '',
          laneId: 9,
          laneEpoch: 1,
        })
      },
    } as unknown as V2ReceiverSessionRuntime
    const factory = new V2BrowserSessionFactory({
      relayBases: ['https://relay.example'],
      capability,
      descriptor: expected,
      descriptorObject: relay.connection.descriptorObject,
      dialRelay: async () => relay.connection,
      openDescriptor: async () => expected,
    })
    const parent = new AbortController()

    await expect(factory.attachRelay(session, parent.signal, 'https://relay.example')).resolves.toMatchObject({ laneId: 9 })
    expect(signals).toHaveLength(2)
    expect(signals[0]).toBe(signals[1])
    expect(signals[0]).not.toBe(parent.signal)
    expect(signals[0]?.aborted).toBe(false)
    expect(vi.getTimerCount()).toBe(0)
    factory.close()
  })

  it('closes an equivocated relay before session construction', async () => {
    const expected = descriptor(1)
    const stale = descriptor(30)
    const relay = new TrackedRelay(4)
    let connectCalls = 0
    const capability: Suite02CapabilityKey = {
      suite: 2,
      readSecret: new Uint8Array(16).fill(1),
      pkHash: new Uint8Array(16).fill(2),
      shareIdRaw: new Uint8Array(12).fill(3),
      shareId: 'share',
    }
    const factory = new V2BrowserSessionFactory({
      relayBases: ['https://relay.example'],
      capability,
      descriptor: expected,
      descriptorObject: relay.connection.descriptorObject,
      dialRelay: async () => relay.connection,
      openDescriptor: async () => stale,
      connectSession: async () => {
        connectCalls += 1
        return {} as V2ReceiverSessionRuntime
      },
    })

    await expect(factory.connectFresh(new AbortController().signal))
      .rejects.toMatchObject({ cause: expect.any(V2StaleShareInstanceError) })
    expect(relay.closeCalls).toBe(1)
    expect(connectCalls).toBe(0)

    factory.close()
  })

  it('pins the byte-exact authenticated descriptor object across relay redials', async () => {
    const expected = descriptor(1)
    const relay = new TrackedRelay(4)
    const capability: Suite02CapabilityKey = {
      suite: 2,
      readSecret: new Uint8Array(16).fill(1),
      pkHash: new Uint8Array(16).fill(2),
      shareIdRaw: new Uint8Array(12).fill(3),
      shareId: 'share',
    }
    const factory = new V2BrowserSessionFactory({
      relayBases: ['https://relay.example'],
      capability,
      descriptor: expected,
      descriptorObject: Uint8Array.of(99),
      dialRelay: async () => relay.connection,
      openDescriptor: async () => expected,
      connectSession: async () => {
        throw new Error('Equivocated descriptor bytes must fail before session construction')
      },
    })

    await expect(factory.connectFresh(new AbortController().signal))
      .rejects.toMatchObject({ cause: expect.objectContaining({ message: expect.stringContaining('authenticated descriptor object') }) })
    expect(relay.closeCalls).toBe(1)

    factory.close()
  })
})

it.each(['all-stopped', 'descriptor-conflict'])('ends fresh recovery for %s authority', async (reason) => {
  const session = new FakeSession([1])
  const factory = new FakeSessionFactory()
  const stopped = new V2RelayReceiverError('stopped', { relayError: {
    code: V2_RELAY_ERROR.stopped, retryAfterMilliseconds: 0,
  } })
  factory.connectFreshImpl = async () => { throw new AggregateError([
    reason === 'all-stopped' ? stopped : new V2StaleShareInstanceError('descriptor conflict'),
    reason === 'all-stopped' ? stopped : new Error('outage'),
  ]) }
  const supervisor = new V2ReceiverReconnectSupervisor({
    descriptor: descriptor(), initial: core(session, new TrackedRelay(1)), sessionFactory: factory,
    policy: 'relay-only', clock: { now: () => 0, sleep: async () => undefined },
  })
  const outcome = supervisor.waitForGenerationAfter(1).then(() => 'recovered', () => 'terminal')
  session.detach(1)
  expect(await outcome).toBe('terminal')
  expect(factory.connectFreshCalls).toBe(1)
  await supervisor.close()
})

it('one stopped relay must not prevent generation recovery through another endpoint', async () => {
  const session = new FakeSession([1])
  const next = new FakeSession([2])
  const relay = new TrackedRelay(1)
  const factory = new FakeSessionFactory()
  factory.connectFreshImpl = async () => {
    if (factory.connectFreshCalls === 1) throw new AggregateError([
      new V2RelayReceiverError('endpoint A stopped', { relayError: { code: V2_RELAY_ERROR.stopped, retryAfterMilliseconds: 0 } }),
      new Error('endpoint B temporarily offline'),
    ])
    return core(next, new TrackedRelay(2))
  }
  const supervisor = new V2ReceiverReconnectSupervisor({
    descriptor: descriptor(), initial: core(session, relay), sessionFactory: factory,
    policy: 'relay-only', clock: { now: () => 0, sleep: async () => undefined },
  })
  const outcome = supervisor.waitForGenerationAfter(1).then(() => 'recovered', () => 'terminal')
  session.detach(1)
  const result = await outcome
  await supervisor.close()
  expect(result).toBe('recovered')
})
