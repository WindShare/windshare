import { afterEach, describe, expect, it, vi } from 'vitest'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { V2BrowserSessionFactory, V2StaleShareInstanceError } from '../../src/receiver/v2-session-factory'
import { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import { V2TranscriptError } from '../../src/session/v2-transcript'
import { V2RelayProtocolError } from '../../src/transport/relay/v2-protocol'
import { FakeSession, FakeSessionFactory, TrackedRelay, core, deferred, descriptor, identity } from './v2-supervisor-fixture'

const HEALTHY_RELAY = 'https://relay.example'
const INVALID_RELAY = 'https://invalid.example'
const INVALID_FRAME = new V2RelayProtocolError('malformed', 'Invalid relay descriptor delivery')
const INVALID_DESCRIPTOR = new SenderObjectError('signature', 'Invalid sender signature')
const INVALID_HANDSHAKE = new V2TranscriptError('Invalid ServerHello signature')
const FAILURES = [
  { stage: 'dial', error: INVALID_FRAME },
  { stage: 'descriptor', error: INVALID_DESCRIPTOR },
  { stage: 'handshake', error: INVALID_HANDSHAKE },
] as const

afterEach(() => vi.useRealTimers())

describe('relay endpoint failure isolation', () => {
  it('does not remember a protocol error from a cancelled connection attempt', async () => {
    const relay = new TrackedRelay(1)
    const share = descriptor()
    const parent = new AbortController()
    const dial = vi.fn(async () => {
      if (dial.mock.calls.length === 1) {
        parent.abort(new DOMException('Connection cancelled', 'AbortError'))
        throw INVALID_FRAME
      }
      return relay.connection
    })
    const factory = new V2BrowserSessionFactory({
      relayBases: [HEALTHY_RELAY], descriptor: share,
      capability: { suite: 2, readSecret: identity(1), pkHash: identity(2),
        shareIdRaw: new Uint8Array(12), shareId: 'share' },
      descriptorObject: relay.connection.descriptorObject, dialRelay: dial,
      openDescriptor: async () => share,
      connectSession: async () => new FakeSession([1]) as unknown as V2ReceiverSessionRuntime,
    })
    try {
      await expect(factory.connectFresh(parent.signal)).rejects.toBe(parent.signal.reason)
      const connected = await factory.connectFresh(new AbortController().signal)
      expect(dial).toHaveBeenCalledTimes(2)
      await connected.session.close()
      await connected.relay.close()
    } finally { factory.close() }
  })

  it('still ends an active session when an extra relay supplies a verified descriptor conflict', async () => {
    vi.useFakeTimers()
    const session = new FakeSession([1])
    const relay = new TrackedRelay(1)
    const factory = new FakeSessionFactory()
    factory.relayBases.push(INVALID_RELAY)
    factory.attachRelayImpl = async () => { throw new V2StaleShareInstanceError('Authenticated descriptor changed') }
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(), initial: core(session, relay), sessionFactory: factory, policy: 'relay-only',
    })
    const states: string[] = []
    supervisor.connection.subscribe(state => states.push(state.kind))
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(states).toEqual(['connected', 'ended'])
      expect(session.isClosed).toBe(true)
      expect(relay.closeCalls).toBe(1)
      expect(factory.closeCalls).toBe(1)
    } finally { await supervisor.close() }
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(FAILURES)('keeps a healthy connection racing after a $stage failure and excludes the endpoint from later generations', async ({ stage, error }) => {
    vi.useFakeTimers()
    const share = descriptor()
    const healthy = new TrackedRelay(1)
    const invalid = new TrackedRelay(2)
    const ready = deferred<void>()
    const calls: string[] = []
    let healthySignal: AbortSignal | undefined
    const factory = new V2BrowserSessionFactory({
      relayBases: [INVALID_RELAY, HEALTHY_RELAY], descriptor: share,
      capability: { suite: 2, readSecret: identity(1), pkHash: identity(2),
        shareIdRaw: new Uint8Array(12), shareId: 'share' },
      descriptorObject: healthy.connection.descriptorObject,
      dialRelay: async (endpoint, _capability, options) => {
        calls.push(endpoint)
        if (endpoint === INVALID_RELAY) {
          if (stage === 'dial') throw error
          return invalid.connection
        }
        healthySignal = options?.signal
        await ready.promise
        return healthy.connection
      },
      openDescriptor: async object => {
        if (stage === 'descriptor' && object === invalid.connection.descriptorObject) throw error
        return share
      },
      connectSession: async options => {
        if (stage === 'handshake' && options.initialChannel === invalid.connection.channel) throw error
        return new FakeSession([1]) as unknown as V2ReceiverSessionRuntime
      },
    })
    // Both endpoints carry the pinned descriptor when testing a handshake failure.
    if (stage === 'handshake') invalid.connection.descriptorObject.set(healthy.connection.descriptorObject)
    const parent = new AbortController()
    const outcome = factory.connectFresh(parent.signal).then(
      value => ({ value }), failure => ({ failure }),
    )
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(healthySignal?.aborted).toBe(false)
      ready.resolve()
      const result = await outcome
      expect(result).toHaveProperty('value')
      if (!('value' in result)) throw result.failure
      expect(result.value.relayBase).toBe(HEALTHY_RELAY)
      await expect(factory.attachRelay(result.value.session, parent.signal, INVALID_RELAY)).rejects.toBe(error)
      await result.value.session.close()
      await result.value.relay.close()

      const next = await factory.connectFresh(parent.signal)
      expect(next.relayBase).toBe(HEALTHY_RELAY)
      expect(calls).toEqual([INVALID_RELAY, HEALTHY_RELAY, HEALTHY_RELAY])
      expect(invalid.closeCalls).toBe(stage === 'dial' ? 0 : 1)
      await next.session.close()
      await next.relay.close()
    } finally {
      parent.abort()
      ready.resolve()
      const result = await outcome
      if ('value' in result) await result.value.session.close()
      factory.close()
    }
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(FAILURES)('preserves an active download when an extra relay reconnects with a $stage failure', async ({ error }) => {
    vi.useFakeTimers()
    const session = new FakeSession([1])
    const healthy = new TrackedRelay(1)
    const invalid = new TrackedRelay(2)
    const factory = new FakeSessionFactory()
    factory.relayBases.push(INVALID_RELAY)
    factory.attachRelayImpl = async () => {
      if (factory.attachRelayCalls > 1) throw error
      session.attach(2)
      return { relay: invalid.connection, laneId: 2 }
    }
    const observed: unknown[] = []
    const trace = vi.fn()
    const supervisor = new V2ReceiverReconnectSupervisor({
      descriptor: descriptor(), initial: core(session, healthy), sessionFactory: factory,
      policy: 'relay-only', protocolTrace: { current: trace }, onRecoveryError: failure => observed.push(failure),
    })
    const states: string[] = []
    supervisor.connection.subscribe(state => states.push(state.kind))
    const content = supervisor.content
    const activation = supervisor.beginConnectivity('download')
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(supervisor.contentLaneCount(activation.routes)).toBe(2)
      session.detach(2)
      await vi.advanceTimersByTimeAsync(0)

      expect(states).toEqual(['connected'])
      expect(session.isClosed).toBe(false)
      expect(healthy.closeCalls).toBe(0)
      expect(invalid.closeCalls).toBe(1)
      expect(factory.closeCalls).toBe(0)
      expect(factory.connectFreshCalls).toBe(0)
      expect(supervisor.generationId).toBe(1)
      expect(supervisor.content).toBe(content)
      expect(activation.routes.active).toBe(true)
      expect(supervisor.contentLaneCount(activation.routes)).toBe(1)
      expect(observed).toEqual([error])
      expect(trace).toHaveBeenCalledWith(expect.objectContaining({
        eventName: 'connection_recovery', transition: 'terminal', relayBase: INVALID_RELAY, generationId: 1,
      }))
      await expect(supervisor.execute(undefined, async generation => generation.session.laneIds()))
        .resolves.toMatchObject({ value: [1] })
      supervisor.requestReconnect()
      await vi.advanceTimersByTimeAsync(60_000)
      expect(factory.attachRelayCalls).toBe(2)
    } finally {
      activation.close()
      await supervisor.close()
    }
    expect(vi.getTimerCount()).toBe(0)
  })
})
