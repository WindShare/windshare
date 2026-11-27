import { afterEach, describe, expect, it, vi } from 'vitest'
import { GenerationRecoveryBudget } from '../../src/receiver/generation-recovery'
import type { ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import { systemReconnectClock } from '../../src/receiver/recovery-clock'
import { ReceiverReconnectSupervisor } from '../../src/receiver/supervisor'
import { BrowserSessionFactory, type ProtocolGenerationCore } from '../../src/receiver/session-factory'
import { RelayRecovery } from '../../src/receiver/relay-selection/recovery'
import type { ReceiverSessionRuntime } from '../../src/session/runtime'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import { RelayReceiverError } from '../../src/transport/relay/receiver'
import { RELAY_ERROR } from '../../src/transport/relay/protocol'
import { core, deferred, descriptor, FakeSession, FakeSessionFactory, identity, TrackedRelay } from './v2-supervisor-fixture'

afterEach(() => vi.useRealTimers())

function fixture(budget = new GenerationRecoveryBudget()) {
  vi.useFakeTimers()
  vi.setSystemTime(0)
  const session = new FakeSession([1])
  const factory = new FakeSessionFactory()
  const states: ReceiverConnectionSnapshot[] = []
  const traces: ProtocolTraceEvent[] = []
  const supervisor = new ReceiverReconnectSupervisor({
    descriptor: descriptor(), initial: core(session, new TrackedRelay(1)), sessionFactory: factory,
    policy: 'relay-only', generationRecovery: budget, generationBackoffMilliseconds: () => 5_000,
    clock: { now: () => Date.now(), sleep: systemReconnectClock.sleep },
    protocolTrace: { current: event => traces.push(event) },
  })
  supervisor.connection.subscribe(state => states.push(state))
  return { session, factory, supervisor, states, traces }
}

describe('reconnect control admission', () => {
  it('rebuilds from the independent relay after the official relay denies access', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const clock = { now: () => Date.now(), sleep: systemReconnectClock.sleep }
    const independent = 'https://relay.example'
    const official = 'https://official.example'
    const share = descriptor()
    const session = new FakeSession([1])
    const relay = new TrackedRelay(1)
    const nextRelay = new TrackedRelay(1)
    const dial = vi.fn(async (base: string) => {
      if (base === official) throw new RelayReceiverError('Access denied', { relayError: {
        code: RELAY_ERROR.accessDenied, retryAfterMilliseconds: 0,
      } })
      if (Date.now() < 1_000) throw new RelayReceiverError('Share is starting', { relayError: {
        code: RELAY_ERROR.starting, retryAfterMilliseconds: 250,
      } })
      return nextRelay.connection
    })
    const factory = new BrowserSessionFactory({
      relayRecovery: new RelayRecovery([independent, official], clock),
      capability: { suite: 2, readSecret: identity(1), pkHash: identity(2),
        shareIdRaw: new Uint8Array(12), shareId: 'share' },
      descriptor: share,
      descriptorObject: relay.connection.descriptorObject,
      dialRelay: dial,
      openDescriptor: async () => share,
      connectSession: async () => new FakeSession([2]) as unknown as ReceiverSessionRuntime,
    })
    const supervisor = new ReceiverReconnectSupervisor({
      descriptor: share, initial: core(session, relay), sessionFactory: factory, clock,
      policy: 'relay-only', generationBackoffMilliseconds: () => 250,
    })
    const states: ReceiverConnectionSnapshot[] = []
    supervisor.connection.subscribe(state => states.push(state))
    try {
      await vi.advanceTimersByTimeAsync(0)
      session.detach(1)
      await vi.advanceTimersByTimeAsync(0)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: {
        kind: 'waiting', reason: 'backoff', retryAt: 250,
      } })
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(249)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: { kind: 'connecting' } })
      expect(dial.mock.calls.map(([base]) => base)).toEqual([official, independent])
      await vi.advanceTimersByTimeAsync(751)
      expect(states.at(-1)).toEqual({ kind: 'connected' })
      expect(states.some(state => state.kind === 'reconnecting' && state.activity.kind === 'waiting' && state.activity.reason === 'server')).toBe(false)
      expect(supervisor.generationId).toBe(2)
      expect(dial.mock.calls.filter(([base]) => base === official)).toHaveLength(1)
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('preserves the denial for active downloads and later previews after reconnecting', async () => {
    const { session, factory, supervisor, states } = fixture()
    const denied = new RelayReceiverError('Access denied', { relayError: {
      code: RELAY_ERROR.accessDenied, retryAfterMilliseconds: 0,
    } })
    factory.connectFreshImpl = async () => { throw denied }
    try {
      const activation = supervisor.beginConnectivity('download')
      const content = supervisor.content.forRoutes(activation.routes)
      session.detach(1)
      await vi.advanceTimersByTimeAsync(0)
      expect(states.at(-1)).toEqual({ kind: 'unavailable', reason: 'access-denied' })
      expect(() => activation.routes.assertActive()).toThrow(denied)
      await expect(content.revisions.open(identity(1))).rejects.toBe(denied)
      expect(() => supervisor.beginConnectivity('preview')).toThrow(denied)
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(60_000)
      expect(factory.connectFreshCalls).toBe(1)
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('keeps depleted capacity waiting through manual and online wakes, then reports one real attempt', async () => {
    const budget = new GenerationRecoveryBudget()
    for (let attempt = 0; attempt < 8; attempt += 1) budget.reserve(0).finish(0)
    const online = new EventTarget()
    vi.stubGlobal('addEventListener', online.addEventListener.bind(online))
    vi.stubGlobal('removeEventListener', online.removeEventListener.bind(online))
    const { session, factory, supervisor, states, traces } = fixture(budget)
    const connected = deferred<ProtocolGenerationCore>()
    factory.connectFreshImpl = () => connected.promise
    try {
      session.detach(1)
      await vi.advanceTimersByTimeAsync(0)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: {
        kind: 'waiting', reason: 'capacity', retryAt: 75_000,
      } })
      expect(factory.connectFreshCalls).toBe(0)
      await vi.advanceTimersByTimeAsync(25_000)
      supervisor.requestReconnect()
      online.dispatchEvent(new Event('online'))
      await vi.advanceTimersByTimeAsync(49_999)
      expect(factory.connectFreshCalls).toBe(0)
      expect(states).toHaveLength(2)
      await vi.advanceTimersByTimeAsync(1)
      expect(factory.connectFreshCalls).toBe(1)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: { kind: 'connecting' } })
      supervisor.requestReconnect()
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(0)
      expect(factory.connectFreshCalls).toBe(1)
      connected.resolve(core(new FakeSession([2]), new TrackedRelay(2)))
      await vi.advanceTimersByTimeAsync(0)
      expect(states.at(-1)).toEqual({ kind: 'connected' })
      expect(traces).toContainEqual(expect.objectContaining({ eventName: 'connection_recovery',
        generationId: 1, transition: 'waiting', waitReason: 'capacity', delayMilliseconds: 75_000 }))
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('keeps share-not-ready polling hints out of the server cooldown banner while honoring the 250ms floor', async () => {
    const { session, factory, supervisor, states, traces } = fixture()
    const connected = deferred<ProtocolGenerationCore>()
    factory.connectFreshImpl = async () => {
      if (factory.connectFreshCalls > 2) return connected.promise
      throw new RelayReceiverError('This share is not ready yet.', { relayError: {
        code: RELAY_ERROR.starting, retryAfterMilliseconds: 250,
      } })
    }
    try {
      session.detach(1)
      await vi.advanceTimersByTimeAsync(0)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: {
        kind: 'waiting', reason: 'backoff', retryAt: 5_000,
      } })
      expect(traces).toContainEqual(expect.objectContaining({ eventName: 'connection_recovery',
        generationId: 1, transition: 'waiting', waitReason: 'share_not_ready', delayMilliseconds: 5_000 }))

      // A manual retry before the 250ms floor gives immediate connecting feedback
      // without redialing early or flashing a service-cooldown notice.
      await vi.advanceTimersByTimeAsync(50)
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(199)
      expect(factory.connectFreshCalls).toBe(1)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: { kind: 'connecting' } })
      await vi.advanceTimersByTimeAsync(1)
      expect(factory.connectFreshCalls).toBe(2)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: {
        kind: 'waiting', reason: 'backoff', retryAt: 5_250,
      } })

      // Once the 250ms floor has elapsed, manual retry skips the remaining backoff immediately.
      await vi.advanceTimersByTimeAsync(300)
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(0)
      expect(factory.connectFreshCalls).toBe(3)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: { kind: 'connecting' } })
      expect(states.some(state => state.kind === 'reconnecting' && state.activity.kind === 'waiting' && state.activity.reason === 'server')).toBe(false)
      connected.resolve(core(new FakeSession([2]), new TrackedRelay(2)))
      await vi.advanceTimersByTimeAsync(0)
      expect(states.at(-1)).toEqual({ kind: 'connected' })
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('enforces the service cooldown and then lets a manual retry skip the remaining backoff', async () => {
    const { session, factory, supervisor, states } = fixture()
    const connected = deferred<ProtocolGenerationCore>()
    factory.connectFreshImpl = async () => {
      if (factory.connectFreshCalls > 1) return connected.promise
      throw new RelayReceiverError('Please retry later', { relayError: {
        code: RELAY_ERROR.admission, retryAfterMilliseconds: 2_000,
      } })
    }
    try {
      session.detach(1)
      await vi.advanceTimersByTimeAsync(0)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: {
        kind: 'waiting', reason: 'server', retryAt: 5_000,
      } })
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(1_999)
      expect(factory.connectFreshCalls).toBe(1)
      await vi.advanceTimersByTimeAsync(1)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: {
        kind: 'waiting', reason: 'backoff', retryAt: 5_000,
      } })
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(0)
      expect(factory.connectFreshCalls).toBe(2)
      expect(states.at(-1)).toEqual({ kind: 'reconnecting', activity: { kind: 'connecting' } })
      connected.resolve(core(new FakeSession([2]), new TrackedRelay(2)))
      await vi.advanceTimersByTimeAsync(0)
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('accepts retries from synchronous observers without replenishing capacity', async () => {
    const { session, factory, supervisor, states } = fixture()
    factory.connectFreshImpl = async () => {
      if (factory.connectFreshCalls <= 8) throw new Error('Network unavailable')
      return core(new FakeSession([2]), new TrackedRelay(2))
    }
    supervisor.connection.subscribe(state => {
      if (state.kind === 'reconnecting' && state.activity.kind === 'waiting' && state.activity.reason === 'backoff') {
        supervisor.requestReconnect()
      }
    })
    try {
      session.detach(1)
      await vi.advanceTimersByTimeAsync(0)
      expect(factory.connectFreshCalls).toBe(8)
      expect(states.at(-1)).toMatchObject({ kind: 'reconnecting', activity: { reason: 'capacity' } })
      await vi.advanceTimersByTimeAsync(75_000)
      expect(factory.connectFreshCalls).toBe(9)
      expect(states.at(-1)).toEqual({ kind: 'connected' })
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('cancels a required wait without starting another handshake', async () => {
    const { session, factory, supervisor } = fixture()
    factory.connectFreshImpl = async () => {
      throw new RelayReceiverError('Please retry later', { relayError: {
        code: RELAY_ERROR.admission, retryAfterMilliseconds: 30_000,
      } })
    }
    session.detach(1)
    await vi.advanceTimersByTimeAsync(0)
    await supervisor.close()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(factory.connectFreshCalls).toBe(1)
    expect(vi.getTimerCount()).toBe(0)
  })
})
