import { describe, expect, it } from 'vitest'

import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import type { Suite02CapabilityKey } from '../../src/crypto/suite02-link'
import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import {
  V2BrowserSessionFactory,
  type V2AttachedRelay,
  type V2ProtocolGenerationCore,
  type V2ReceiverSessionFactory,
  V2StaleShareInstanceError,
} from '../../src/receiver/v2-session-factory'
import { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import { V2SessionRuntimeError, type V2LaneChange } from '../../src/session/v2-runtime-types'
import type { V2RelayReceiverConnection } from '../../src/transport/relay/v2-receiver'

class FakeSession {
  readonly initialLaneId: number
  readonly #laneIds: Set<number>
  readonly #listeners = new Set<(change: V2LaneChange) => void>()
  closeCalls = 0
  isClosed = false

  constructor(laneIds: readonly number[]) {
    if (laneIds.length === 0) throw new Error('A fake generation needs an initial lane')
    this.initialLaneId = laneIds[0]!
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
  attachRelayCalls = 0
  connectFreshCalls = 0
  closeCalls = 0
  attachRelayImpl: (
    session: V2ReceiverSessionRuntime,
    signal: AbortSignal,
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
  ): Promise<V2AttachedRelay> {
    this.attachRelayCalls += 1
    return this.attachRelayImpl(session, signal)
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
) {
  const supervisor = new V2ReceiverReconnectSupervisor({
    descriptor: share,
    initial: core(session, relay),
    sessionFactory: factory,
    randomBytes: (length) => new Uint8Array(length).fill(9),
  })
  return { factory, supervisor }
}

async function flushReconciliation(): Promise<void> {
  for (let turn = 0; turn < 16; turn += 1) await Promise.resolve()
}

describe('v2 receiver reconnect supervisor', () => {
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

    session.detach(1)
    await flushReconciliation()

    expect(factory.attachRelayCalls).toBe(1)
    expect(factory.connectFreshCalls).toBe(0)
    expect(supervisor.generationId).toBe(1)
    expect(session.laneIds()).toEqual([2, 3])
    expect(initialRelay.closeCalls).toBe(1)

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

    await supervisor.close()
  })

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

describe('v2 browser session factory descriptor continuity', () => {
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
      relayBase: 'https://relay.example',
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
      .rejects.toBeInstanceOf(V2StaleShareInstanceError)
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
      relayBase: 'https://relay.example',
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
      .rejects.toThrow('authenticated descriptor object')
    expect(relay.closeCalls).toBe(1)

    factory.close()
  })
})
