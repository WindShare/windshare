import { afterEach, describe, expect, it, vi } from 'vitest'
import { GenerationRecoveryBudget } from '../../src/receiver/generation-recovery'
import type { ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import { systemReconnectClock } from '../../src/receiver/recovery-clock'
import { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import type { V2ProtocolGenerationCore } from '../../src/receiver/v2-session-factory'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'
import { V2RelayReceiverError } from '../../src/transport/relay/v2-receiver'
import { V2_RELAY_ERROR } from '../../src/transport/relay/v2-protocol'
import { core, deferred, descriptor, FakeSession, FakeSessionFactory, TrackedRelay } from './v2-supervisor-fixture'

afterEach(() => vi.useRealTimers())

function fixture(budget = new GenerationRecoveryBudget()) {
  vi.useFakeTimers()
  vi.setSystemTime(0)
  const session = new FakeSession([1])
  const factory = new FakeSessionFactory()
  const states: ReceiverConnectionSnapshot[] = []
  const traces: V2ProtocolTraceEvent[] = []
  const supervisor = new V2ReceiverReconnectSupervisor({
    descriptor: descriptor(), initial: core(session, new TrackedRelay(1)), sessionFactory: factory,
    policy: 'relay-only', generationRecovery: budget, generationBackoffMilliseconds: () => 5_000,
    clock: { now: () => Date.now(), sleep: systemReconnectClock.sleep },
    protocolTrace: { current: event => traces.push(event) },
  })
  supervisor.connection.subscribe(state => states.push(state))
  return { session, factory, supervisor, states, traces }
}

describe('reconnect control admission', () => {
  it('keeps depleted capacity waiting through manual and online wakes, then reports one real attempt', async () => {
    const budget = new GenerationRecoveryBudget()
    for (let attempt = 0; attempt < 8; attempt += 1) budget.reserve(0).finish(0)
    const online = new EventTarget()
    vi.stubGlobal('addEventListener', online.addEventListener.bind(online))
    vi.stubGlobal('removeEventListener', online.removeEventListener.bind(online))
    const { session, factory, supervisor, states, traces } = fixture(budget)
    const connected = deferred<V2ProtocolGenerationCore>()
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

  it('enforces the service cooldown and then lets a manual retry skip the remaining backoff', async () => {
    const { session, factory, supervisor, states } = fixture()
    const connected = deferred<V2ProtocolGenerationCore>()
    factory.connectFreshImpl = async () => {
      if (factory.connectFreshCalls > 1) return connected.promise
      throw new V2RelayReceiverError('Please retry later', { relayError: {
        code: V2_RELAY_ERROR.admission, retryAfterMilliseconds: 2_000,
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
      throw new V2RelayReceiverError('Please retry later', { relayError: {
        code: V2_RELAY_ERROR.admission, retryAfterMilliseconds: 30_000,
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
