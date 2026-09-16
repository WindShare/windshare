import { afterEach, describe, expect, it, vi } from 'vitest'
import { systemReconnectClock } from '../../src/receiver/recovery-clock'
import { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'
import { core, descriptor, FakeSession, FakeSessionFactory, TrackedRelay } from './v2-supervisor-fixture'

afterEach(() => vi.useRealTimers())

function fixture(jitter = 0.5) {
  vi.useFakeTimers()
  vi.setSystemTime(0)
  vi.spyOn(Math, 'random').mockReturnValue(jitter)
  const session = new FakeSession([1])
  const factory = new FakeSessionFactory()
  const traces: V2ProtocolTraceEvent[] = []
  const supervisor = new V2ReceiverReconnectSupervisor({
    descriptor: descriptor(), initial: core(session, new TrackedRelay(1)), sessionFactory: factory,
    policy: 'relay-only', clock: { now: () => Date.now(), sleep: systemReconnectClock.sleep },
    protocolTrace: { current: event => traces.push(event) },
  })
  return { session, factory, supervisor, traces }
}

describe('automatic generation recovery pacing', () => {
  it.each([
    { outage: 50, recoveryDeadline: 150 },
    { outage: 250, recoveryDeadline: 500 },
    { outage: 2_500, recoveryDeadline: 5_000 },
    { outage: 10_000, recoveryDeadline: 15_000 },
    { outage: 30_000, recoveryDeadline: 40_000 },
    { outage: 70_000, recoveryDeadline: 100_000 },
  ])('recovers a $outage ms outage without a manual or online wake', async ({ outage, recoveryDeadline }) => {
    const { session, factory, supervisor, traces } = fixture()
    let connectedAt: number | undefined
    factory.connectFreshImpl = async () => {
      if (Date.now() < outage) throw new Error('Relay connection rejected')
      connectedAt = Date.now()
      return core(new FakeSession([2]), new TrackedRelay(2))
    }
    const recovered = supervisor.waitForGenerationAfter(1)
    try {
      session.detach(1)
      await vi.advanceTimersByTimeAsync(0)
      expect(factory.connectFreshCalls).toBe(1)
      await vi.advanceTimersByTimeAsync(outage - 1)
      expect(connectedAt).toBeUndefined()
      await vi.advanceTimersByTimeAsync(recoveryDeadline - outage + 1)
      expect(connectedAt).toBeGreaterThanOrEqual(outage)
      expect(connectedAt).toBeLessThanOrEqual(recoveryDeadline)
      await recovered
      expect(supervisor.generationId).toBe(2)
      expect(traces).toContainEqual(expect.objectContaining({
        eventName: 'connection_recovery', transition: 'connected',
      }))
    } finally {
      recovered.catch(() => undefined)
      await supervisor.close()
    }
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each([0, 1])('bounds short-outage recovery at jitter extreme %s', async jitter => {
    const { session, factory, supervisor } = fixture(jitter)
    factory.connectFreshImpl = async () => {
      if (Date.now() < 2_500) throw new Error('Relay connection rejected')
      return core(new FakeSession([2]), new TrackedRelay(2))
    }
    try {
      session.detach(1)
      await vi.advanceTimersByTimeAsync(2_500)
      expect(supervisor.generationId).toBe(1)
      await vi.advanceTimersByTimeAsync(2_500)
      expect(supervisor.generationId).toBe(2)
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('retains admission capacity across repeated successful reconnects', async () => {
    const { session, factory, supervisor } = fixture()
    let active = session
    factory.connectFreshImpl = async () => {
      active = new FakeSession([factory.connectFreshCalls + 1])
      return core(active, new TrackedRelay(active.initialLaneId))
    }
    try {
      for (let outage = 0; outage < 8; outage += 1) {
        active.detach(active.initialLaneId)
        await vi.advanceTimersByTimeAsync(0)
      }
      expect(factory.connectFreshCalls).toBe(8)
      expect(supervisor.generationId).toBe(9)
      active.detach(active.initialLaneId)
      await vi.advanceTimersByTimeAsync(74_999)
      expect(factory.connectFreshCalls).toBe(8)
      await vi.advanceTimersByTimeAsync(1)
      expect(factory.connectFreshCalls).toBe(9)
      expect(supervisor.generationId).toBe(10)
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('spreads automatic attempts across the first minute and keeps the long-term budget', async () => {
    const { session, factory, supervisor, traces } = fixture()
    const attempts: number[] = []
    factory.connectFreshImpl = async () => {
      attempts.push(Date.now())
      throw new Error('Relay connection rejected')
    }
    try {
      session.detach(1)
      await vi.advanceTimersByTimeAsync(10_000)
      expect(attempts.length).toBeLessThanOrEqual(6)
      await vi.advanceTimersByTimeAsync(50_000)
      expect(attempts.length).toBeGreaterThanOrEqual(7)
      expect(attempts.length).toBeLessThanOrEqual(8)
      expect(attempts.at(-1)).toBeGreaterThanOrEqual(30_000)
      await vi.advanceTimersByTimeAsync(540_000)
      expect(attempts.length).toBeLessThanOrEqual(16)
      expect(traces).toContainEqual(expect.objectContaining({
        eventName: 'connection_recovery', transition: 'waiting', phase: 'waiting', waitReason: 'capacity',
      }))
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })
})
