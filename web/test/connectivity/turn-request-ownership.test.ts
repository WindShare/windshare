import { expect, it } from 'vitest'
import type { TURNTraceStage } from '../../src/connectivity/diagnostics'
import { TURNPaths } from '../../src/connectivity/turn-paths'
import { decodeTURNDemand, encodeTURNGrants, type TURNDemand, type TURNStatus } from '../../src/protocol/turn-control'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import { EnvelopeSealer } from '../../src/session/envelope'
import { createPeerAttemptIdentity } from '../../src/session/identities'
import { encodeBody, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { PeerSlotPool } from '../../src/session/peer-slots/slots'
import { BINDING, deferred, KEY, openSent, runtimeFixture } from '../session/v2-send-fixture'

const demand = {
  queuedBytes: 1, queuedBlocks: 1, networkWaitingBytes: 1, capacityWaitingBytes: 0, unfinishedBytes: 0,
  inFlightBytes: 0, remainingBytes: 8 * 1024 * 1024, remainingKnown: true, usefulBytes: 0, outputBytes: 0,
}

function fixture(onParallelReply: (manager: TURNPaths) => void) {
  let now = 0
  let receive!: (body: Uint8Array<ArrayBuffer>) => void
  const requests: TURNDemand[] = [], started: string[] = [], admitted: string[] = []
  const lanes = new Map([[1, 0]])
  const manager = new TURNPaths({
    slots: new PeerSlotPool(), now: () => now, identity: () => new Uint8Array(16).fill(1),
    hasDelivered: () => false,
    session: {
      subscribeTURNControls: (callback) => { receive = callback; return () => {} },
      sendTURNControl: async (body) => { requests.push(decodeTURNDemand(body)) },
      availableLaneIds: () => [...lanes.keys()], laneEpoch: id => lanes.get(id), retireLaneIfCovered: () => undefined,
    },
    start: (grant) => {
      started.push(grant.serviceId)
      const lane = { laneId: 2, laneEpoch: 0 }
      lanes.set(lane.laneId, lane.laneEpoch)
      return {
        attemptId: createPeerAttemptIdentity(new Uint8Array(16).fill(2)),
        result: Promise.resolve({ type: 'admitted', lane }), cancel: () => {},
      }
    },
    release: async lane => { lanes.delete(lane.laneId) },
    trace: (stage, detail) => {
      if (stage === 'turn_grants_received' && detail.mode === 'parallel') onParallelReply(manager)
      if (stage === 'turn_path_admitted' && detail.serviceId) admitted.push(detail.serviceId)
    },
  })
  return {
    manager, requests, started, admitted,
    step: (time: number) => { now = time; manager.step(demand) },
    reply: (request: TURNDemand, status: TURNStatus) => receive(encodeTURNGrants({
      requestId: request.requestId, status, hasFallback: true,
      grants: status !== 'partial_retryable' ? [] : [{
        serviceId: 'home', grantId: new Uint8Array(16).fill(3), urls: ['turn:localhost'],
        username: 'u', password: 'p', validForMs: 0n,
      }],
    })),
  }
}

it.each(['ok', 'retry_later', 'partial_retryable'] as const)('charges a reentrant %s reply to its original full request', async (status) => {
  const f = fixture(manager => manager.step(demand))
  try {
    f.step(0)
    f.step(3000)
    expect(f.requests.map(r => r.mode)).toEqual(['parallel'])
    f.reply(f.requests[0]!, status)
    f.step(3000)
    expect(f.requests.map(r => r.mode)).toEqual(['parallel', 'all'])
    f.reply(f.requests[1]!, 'retry_later')
    f.step(5000)
    expect(f.requests.map(r => r.mode)).toEqual(['parallel', 'all', 'all'])
    await new Promise<void>(resolve => queueMicrotask(resolve))
    const expected = status === 'partial_retryable' ? ['home'] : []
    expect(f.started).toEqual(expected)
    expect(f.admitted).toEqual(expected)
  } finally {
    await f.manager.reset()
  }
})

it('discards prior grants when a reply observer ends the activation', async () => {
  const f = fixture(manager => { void manager.reset() })
  try {
    f.step(0)
    const old = f.requests[0]!
    f.reply(old, 'partial_retryable')
    await f.manager.reset()
    f.step(1000)
    f.reply(old, 'partial_retryable')
    expect(f.requests.map(r => r.mode)).toEqual(['parallel', 'parallel'])
    expect(f.started).toEqual([])
    expect(f.admitted).toEqual([])
  } finally {
    await f.manager.reset()
  }
})

async function flushMicrotasks(turns = 6): Promise<void> {
  for (let i = 0; i < turns; i += 1) await Promise.resolve()
}

function queuedFixture(start?: ConstructorParameters<typeof TURNPaths>[0]['start']) {
  const events: ProtocolTraceEvent[] = []
  const traces: Array<{ stage: TURNTraceStage; status?: string; mode?: string; serviceId?: string }> = []
  const waiters: Array<{ predicate: () => boolean; resolve: () => void }> = []
  const notify = () => {
    for (let i = waiters.length - 1; i >= 0; i -= 1) {
      if (waiters[i]!.predicate()) waiters.splice(i, 1)[0]!.resolve()
    }
  }
  const { runtime, channel } = runtimeFixture({
    current: (event) => {
      events.push(event)
      notify()
    },
  })
  let now = 0
  const manager = new TURNPaths({
    session: runtime,
    slots: new PeerSlotPool(),
    now: () => now,
    identity: () => new Uint8Array(16).fill(1),
    hasDelivered: () => false,
    start: start ?? (() => ({
      attemptId: createPeerAttemptIdentity(new Uint8Array(16).fill(2)),
      result: Promise.resolve({ type: 'lifecycle-cancelled', owner: 'last-activation' }),
      cancel: () => {},
    })),
    release: async () => {},
    trace: (stage, detail) => {
      traces.push({ stage, ...detail })
      notify()
    },
  })
  return {
    runtime, channel, events, traces, manager,
    step: (time: number, usefulBytes = 0) => {
      now = time
      manager.step({ ...demand, usefulBytes })
    },
    waitUntil: (predicate: () => boolean): Promise<void> => {
      if (predicate()) return Promise.resolve()
      return new Promise<void>(resolve => { waiters.push({ predicate, resolve }) })
    },
    sentTURNDemands: async () => (await openSent(channel))
      .filter(entry => entry.message.kind === MESSAGE_KIND.turnControl)
      .map(entry => decodeTURNDemand(entry.message.body)),
  }
}

it('withdraws timed-out queued TURN requests from the real session writer without double-charging backoff', async () => {
  const f = queuedFixture()
  try {
    const blocker = f.runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    await f.channel.sending.promise

    const MiB = 1024 * 1024
    f.step(0, 0)
    f.step(2000, 4 * MiB)
    f.step(4000, 8 * MiB)
    f.step(6000, 12 * MiB)
    f.step(8000, 16 * MiB)
    await flushMicrotasks()

    f.step(9999, 20 * MiB)
    f.step(10_000, 20 * MiB)

    const sentinel = f.runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    f.channel.unblock()
    await Promise.all([blocker, sentinel])

    expect(await f.sentTURNDemands()).toEqual([{ requestId: 2n, mode: 'parallel', serviceIds: [] }])
    expect(f.channel.state).toBe('open')
    expect(f.runtime.availableLaneIds()).toEqual([1])
    expect(f.events.some(event => event.eventName === 'protocol_operation' &&
      event.requestKind === 'turn_control' && event.transition === 'send_withdrawn')).toBe(true)
  } finally {
    await f.manager.reset()
    await f.runtime.close()
  }
})

it('withdraws queued TURN requests on reset while leaving the session lane open', async () => {
  const f = queuedFixture()
  try {
    const blocker = f.runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    await f.channel.sending.promise

    f.step(0)
    await f.manager.reset()

    const followUp = f.runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    f.channel.unblock()
    await Promise.all([blocker, followUp])

    const sent = await openSent(f.channel)
    expect(sent.map(entry => [entry.sequence, entry.message.kind])).toEqual([
      [0n, MESSAGE_KIND.listChildren],
      [1n, MESSAGE_KIND.listChildren],
    ])
    expect(f.channel.state).toBe('open')
    expect(f.runtime.availableLaneIds()).toEqual([1])
    expect(f.events.some(event => event.eventName === 'protocol_operation' &&
      event.requestKind === 'turn_control' && event.transition === 'send_withdrawn')).toBe(true)
  } finally {
    await f.manager.reset()
    await f.runtime.close()
  }
})

it('withdraws other queued TURN requests when an authenticated reply marks TURN unconfigured', async () => {
  const second = deferred<{ type: 'lifecycle-cancelled'; owner: 'last-activation' }>()
  const f = queuedFixture((grant) => ({
    attemptId: createPeerAttemptIdentity(new Uint8Array(16).fill(2)),
    result: grant.serviceId === 'b'
      ? second.promise
      : Promise.resolve({ type: 'lifecycle-cancelled', owner: 'last-activation' }),
    cancel: () => second.resolve({ type: 'lifecycle-cancelled', owner: 'last-activation' }),
  }))
  try {
    const server = new EnvelopeSealer(KEY, { ...BINDING, direction: 1 })
    const deliver = async (requestId: bigint, status: TURNStatus, services: string[] = []) => {
      const grants = encodeTURNGrants({
        requestId, status, hasFallback: false,
        grants: services.map((serviceId, index) => ({
          serviceId, grantId: new Uint8Array(16).fill(index + 3), urls: ['turn:localhost'],
          username: 'u', password: 'p', validForMs: 0n,
        })),
      })
      f.channel.receive(await server.seal(encodeMessage(MESSAGE_KIND.turnControl, undefined, grants).plaintext))
    }
    const turnEvents = (transition: string) => f.events.filter(event =>
      event.eventName === 'protocol_operation' && event.requestKind === 'turn_control' && event.transition === transition,
    )

    // WebCrypto sealing/opening resolves asynchronously on worker threads, so wait for
    // observed send completion and authenticated grant receipt instead of timer ticks.
    f.channel.unblock()
    f.step(0)
    await f.waitUntil(() => turnEvents('send_completed').length === 1)
    await deliver(1n, 'ok', ['a', 'b'])
    await f.waitUntil(() => f.traces.some(trace => trace.stage === 'turn_grants_received' && trace.status === 'ok'))
    await flushMicrotasks()

    f.step(2000)
    await f.waitUntil(() => turnEvents('send_completed').length === 2)
    second.resolve({ type: 'lifecycle-cancelled', owner: 'last-activation' })
    await flushMicrotasks()

    f.channel.block()
    const blocker = f.runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    await f.channel.sending.promise

    f.step(4000)
    await deliver(2n, 'unconfigured')
    await f.waitUntil(() => f.traces.some(trace => trace.stage === 'turn_grants_received' && trace.status === 'unconfigured'))

    const followUp = f.runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    f.channel.unblock()
    await Promise.all([blocker, followUp])

    expect(await f.sentTURNDemands()).toEqual([
      { requestId: 1n, mode: 'parallel', serviceIds: [] },
      { requestId: 2n, mode: 'all', serviceIds: ['a'] },
    ])
    expect(f.channel.state).toBe('open')
    expect(turnEvents('send_withdrawn')).toHaveLength(2)
  } finally {
    await f.manager.reset()
    await f.runtime.close()
  }
})
