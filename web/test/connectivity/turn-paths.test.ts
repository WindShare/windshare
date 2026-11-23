import { expect, it } from 'vitest'
import { TURNPaths } from '../../src/connectivity/turn-paths'
import { TURNDeliveryPressure } from '../../src/connectivity/turn-demand'
import type { TURNTraceStage } from '../../src/connectivity/diagnostics'
import type { PeerAdmittedLane, PeerAttemptResult } from '../../src/connectivity/peer-failure'
import { createPeerAttemptIdentity, type PeerPathIdentity } from '../../src/session/identities'
import { PeerSlotPool, type PeerAttemptOrdinal } from '../../src/session/peer-slots/slots'
import { decodeTURNDemand, encodeTURNGrants, type TURNDemand, type TURNStatus } from '../../src/protocol/turn-control'

const network = {
  queuedBytes: 1, queuedBlocks: 1, networkWaitingBytes: 1, capacityWaitingBytes: 0, unfinishedBytes: 0,
  inFlightBytes: 0, remainingBytes: 8 * 1024 * 1024, remainingKnown: true, usefulBytes: 0, outputBytes: 0,
}
const settle = () => new Promise<void>((resolve) => queueMicrotask(() => queueMicrotask(() => queueMicrotask(resolve))))
const MiB = 1024 * 1024

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}

function fixture(
  release?: (lane: PeerAdmittedLane) => Promise<void>,
  onTrace?: (stage: TURNTraceStage, lane?: PeerAdmittedLane) => void,
) {
  let now = 1000, value = 0
  let activeLanes = new Set([1])
  const epochs = new Map([[1, 0]])
  let listener: ((body: Uint8Array<ArrayBuffer>) => void) | undefined
  const usable = (lane: PeerAdmittedLane) => activeLanes.has(lane.laneId) && epochs.get(lane.laneId) === lane.laneEpoch
  const retireLaneIfCovered = (old: PeerAdmittedLane, successor?: PeerAdmittedLane) => {
    if (successor) {
      if (!usable(successor) || (successor.laneId === old.laneId && successor.laneEpoch === old.laneEpoch)) return
    } else if (usable(old) && activeLanes.size < 2) return
    if (usable(old)) activeLanes.delete(old.laneId)
    return Promise.resolve()
  }
  const slots = new PeerSlotPool()
  const requests: TURNDemand[] = []
  const attempts: Array<{ ordinal: PeerAttemptOrdinal; path: PeerPathIdentity; finish: (result: PeerAttemptResult) => void }> = []
  const released: number[] = []
  const traces: Array<{ stage: TURNTraceStage; grantId?: string; reason?: string; path?: PeerPathIdentity; lane?: PeerAdmittedLane }> = []
  const manager = new TURNPaths({
    now: () => now, slots, identity: () => new Uint8Array(16).fill(++value),
    session: {
      subscribeTURNControls: (cb) => { listener = cb; return () => { listener = undefined } },
      sendTURNControl: async (b) => { requests.push(decodeTURNDemand(b)) },
      availableLaneIds: () => [...activeLanes],
      laneEpoch: (id) => epochs.get(id),
      retireLaneIfCovered,
    },
    start: (_grant, path, ordinal) => {
      const { promise, resolve } = deferred<PeerAttemptResult>()
      attempts.push({ ordinal, path, finish: (result) => {
        if (result.type === 'admitted') {
          epochs.set(result.lane.laneId, result.lane.laneEpoch)
          activeLanes.add(result.lane.laneId)
        }
        resolve(result)
      } })
      return {
        attemptId: createPeerAttemptIdentity(new Uint8Array(16).fill(3)),
        result: promise, cancel: (owner) => resolve({ type: 'lifecycle-cancelled', owner }),
      }
    },
    release: async (lane) => {
      released.push(lane.laneId)
      if (epochs.get(lane.laneId) === lane.laneEpoch) activeLanes.delete(lane.laneId)
      await release?.(lane)
    },
    trace: (stage, detail, path, lane) => {
      traces.push({ stage, ...(detail.grantId && { grantId: detail.grantId }), ...(detail.reason && { reason: detail.reason }),
        ...(path && { path }), ...(lane && { lane }) })
      onTrace?.(stage, lane)
    },
  })
  return {
    manager, slots, attempts, requests, released, traces, epochs, retireLaneIfCovered,
    step: (elapsed: number, d = network) => { now += elapsed; manager.step(d) },
    available: (...ids: number[]) => { activeLanes = new Set(ids) },
    reply: (status: TURNStatus = 'ok', services = status === 'ok' ? ['home'] : [], ttl = 60_000n, request = requests.at(-1)!) =>
      listener!(encodeTURNGrants({ requestId: request.requestId, status, hasFallback: true,
        grants: services.map(serviceId => ({
          serviceId, grantId: new Uint8Array(16).fill(++value), urls: ['turn:localhost'],
          username: 'u', password: 'p', validForMs: ttl,
        })),
      })),
  }
}

it('reuses a slot for ordinary renewal and authenticates a spare before retiring the last lane', async () => {
  const f = fixture()
  f.step(0)
  f.reply()
  const first = f.attempts[0]!
  first.finish({ type: 'admitted', lane: { laneId: 2, laneEpoch: 1 } })
  await settle()
  f.available(1, 2)
  f.step(46_000)
  expect(f.requests.at(-1)!.serviceIds).toEqual(['home'])
  f.reply()
  await settle()
  const second = f.attempts[1]!
  expect(second.path).toBe(first.path)
  expect(second.ordinal).toEqual({ peerSlot: first.ordinal.peerSlot, attemptSequence: 2n })
  expect(f.released).toEqual([2])
  second.finish({ type: 'admitted', lane: { laneId: 3, laneEpoch: 1 } })
  await settle()
  f.available(3)
  f.step(46_000)
  f.reply()
  await settle()
  const spare = f.attempts[2]!
  expect(spare.ordinal.peerSlot).not.toBe(second.ordinal.peerSlot)
  expect(spare.path).not.toBe(second.path)
  expect(f.released).toEqual([2])
  spare.finish({ type: 'lifecycle-cancelled', owner: 'last-activation' })
  await settle()
  expect(f.released).toEqual([2])
  f.step(2000)
  f.reply()
  await settle()
  const successor = f.attempts[3]!
  expect(successor.path).toBe(spare.path)
  expect(successor.ordinal.attemptSequence).toBe(2n)
  successor.finish({ type: 'admitted', lane: { laneId: 4, laneEpoch: 1 } })
  await settle()
  expect(f.released).toEqual([2, 3])
  await f.manager.reset()
  expect(f.released).toEqual([2, 3, 4])
  const closed = f.traces.filter(t => t.stage === 'turn_path_closed')
  expect(closed.map(t => t.grantId)).toEqual(f.traces.filter(t => t.stage === 'turn_path_admitted').map(t => t.grantId))
  expect(closed.map(t => ({ stage: t.stage, path: t.path, reason: t.reason, lane: t.lane }))).toEqual([
    { stage: 'turn_path_closed', reason: 'owner_closed', path: first.path, lane: { laneId: 2, laneEpoch: 1 } },
    { stage: 'turn_path_closed', reason: 'owner_closed', path: second.path, lane: { laneId: 3, laneEpoch: 1 } },
    { stage: 'turn_path_closed', reason: 'owner_closed', path: successor.path, lane: { laneId: 4, laneEpoch: 1 } },
  ])
})
it.each(['ordinary', 'spare'] as const)('claims the old lane before a reentrant %s renewal trace', async (renewal) => {
  const old = { laneId: 2, laneEpoch: 1 }
  const next = { laneId: 3, laneEpoch: 2 }
  const covering = renewal === 'ordinary' ? { laneId: 1, laneEpoch: 0 } : next
  const reciprocal: Array<Promise<void> | undefined> = []
  const f = fixture(undefined, (stage, lane) => {
    if (stage === 'turn_path_closed' && lane?.laneId === old.laneId) reciprocal.push(f.retireLaneIfCovered(covering))
  })
  f.step(0); f.reply()
  f.attempts[0]!.finish({ type: 'admitted', lane: old })
  await settle()
  if (renewal === 'spare') f.available(old.laneId)
  f.step(46_000); f.reply()
  await settle()
  if (renewal === 'spare') {
    expect(reciprocal).toEqual([])
    f.attempts[1]!.finish({ type: 'admitted', lane: next })
    await settle()
  }
  expect(reciprocal).toEqual([undefined])
  expect(f.released).toEqual([old.laneId])
  await f.manager.reset()
})

it('keeps fallback idle under sink pressure and bounds retryable demand', () => {
  const f = fixture()
  f.step(0, { ...network, networkWaitingBytes: 0, capacityWaitingBytes: 1 })
  f.step(6000, { ...network, networkWaitingBytes: 0, capacityWaitingBytes: 1 })
  expect(f.requests).toHaveLength(0)
  f.step(0)
  f.reply('retry_later')
  for (const delay of [2000, 10_000, 30_000]) {
    f.step(delay)
    f.reply('retry_later')
  }
  f.step(60_000); f.reply('retry_later')
  f.step(30_000); f.reply('retry_later')
  f.step(60_000)
  expect(f.requests.map((d) => d.mode)).toEqual(['parallel', 'parallel', 'all', 'all', 'all', 'all'])
})

it.each([1000, 1001])('measures five seconds of completed blocks with %ims sampling', (tick) => {
  for (const [blocks, wanted] of [[(i: number) => Math.floor(i / 2.5), 'all'], [(i: number) => Math.floor(i / 2) * 4, 'parallel']] as const) {
    const pressure = new TURNDeliveryPressure()
    for (let i = 0; i <= 10; i++) {
      expect(pressure.observe(i * tick, { ...network, usefulBytes: blocks(i) * MiB })).toBe(i * tick < 5000 ? 'parallel' : wanted)
    }
  }
})

it('keeps same-tick bytes for the next advancing observation and clears pressure under sink backpressure', () => {
  const pressure = new TURNDeliveryPressure()
  const delivered = { ...network, usefulBytes: MiB }
  pressure.observe(0, network)
  pressure.observe(1000, network)
  pressure.observe(1000, delivered)
  pressure.observe(2000, delivered)
  expect(pressure.observe(4000, delivered)).toBe('parallel')
  expect(pressure.observe(5000, delivered)).toBe('all')
  expect(pressure.observe(8000, { ...delivered, networkWaitingBytes: 0, capacityWaitingBytes: 1 })).toBeUndefined()
  expect(pressure.observe(20_000, delivered)).toBe('parallel')
})

it('gives fallback a fresh budget after parallel failures and waits for an outstanding full request', () => {
  const f = fixture()
  let usefulBytes = 0
  f.step(0)
  f.reply('retry_later')
  for (const delay of [2000, 10_000, 30_000]) {
    usefulBytes += delay * 4096
    f.step(delay, { ...network, usefulBytes })
    f.reply('retry_later')
  }
  f.step(3000, { ...network, usefulBytes })
  expect(f.requests.map(r => r.mode)).toEqual(['parallel', 'parallel', 'parallel', 'parallel', 'all'])

  const pending = fixture()
  pending.step(0)
  pending.step(3000)
  expect(pending.requests).toHaveLength(1)
  pending.step(5000)
  expect(pending.requests.map(r => r.mode)).toEqual(['parallel', 'all'])
  pending.reply('retry_later', [], 0n, pending.requests[0])
  pending.step(1000)
  expect(pending.requests).toHaveLength(2)
})

it('admits partial grants immediately and retries missing services only under eligible pressure', async () => {
  const f = fixture()
  f.step(0)
  f.reply('ok', ['home'], 0n)
  f.attempts[0]!.finish({ type: 'admitted', lane: { laneId: 2, laneEpoch: 1 } })
  await settle()
  f.step(3000)
  f.reply('partial_retryable', ['home', 'extra'], 0n)
  expect(f.attempts).toHaveLength(2)
  f.attempts[1]!.finish({ type: 'admitted', lane: { laneId: 3, laneEpoch: 1 } })
  await settle()
  f.step(2000, { ...network, networkWaitingBytes: 0 })
  expect(f.requests).toHaveLength(2)
  f.step(0)
  f.step(3000)
  expect(f.requests.map(r => r.mode)).toEqual(['parallel', 'all', 'all'])
  f.reply('ok', ['home', 'extra', 'fallback'], 0n)
  expect(f.attempts).toHaveLength(3)
  await f.manager.reset()
})

it.each(['detached', 'new epoch'])('keeps the last working lane when its replacement is %s at handoff', async (loss) => {
  const f = fixture()
  const old = { laneId: 2, laneEpoch: 1 }, next = { laneId: 3, laneEpoch: 2 }
  f.step(0); f.reply()
  f.attempts[0]!.finish({ type: 'admitted', lane: old })
  await settle()
  f.available(old.laneId)
  f.step(46_000); f.reply()
  const spare = f.attempts[1]!
  expect(spare.ordinal.peerSlot).not.toBe(f.attempts[0]!.ordinal.peerSlot)
  spare.finish({ type: 'admitted', lane: next })
  if (loss === 'detached') f.manager.detached(spare.path, next)
  else f.epochs.set(next.laneId, next.laneEpoch + 1)
  await settle()
  expect(f.released).toEqual([next.laneId])
  expect(f.traces.filter(t => t.stage === 'turn_path_admitted')).toHaveLength(1)
  f.step(2000)
  expect(f.requests.at(-1)?.serviceIds).toEqual(['home'])
  await f.manager.reset()
})

it('suppresses admission when an old-lane withdrawal callback invalidates its successor', async () => {
  const old = { laneId: 2, laneEpoch: 1 }
  const next = { laneId: 3, laneEpoch: 2 }
  const f = fixture(async removed => {
    if (removed.laneId === old.laneId) {
      f.available()
      f.manager.detached(f.attempts[1]!.path, next)
    }
  })
  f.step(0); f.reply()
  f.attempts[0]!.finish({ type: 'admitted', lane: old })
  await settle()
  f.available(old.laneId)
  f.step(46_000); f.reply()
  f.attempts[1]!.finish({ type: 'admitted', lane: next })
  await settle()
  expect(f.traces.filter(t => t.stage === 'turn_path_admitted').map(t => t.lane)).toEqual([old])
  expect(f.released).toEqual([old.laneId, next.laneId])
  await f.manager.reset()
  const leases = Array.from({ length: 8 }, () => f.slots.acquire())
  leases.forEach(lease => lease.close())
})

it('reuses the ordinary slot when the only available lane belongs to another path', async () => {
  const f = fixture()
  f.step(0); f.reply()
  const old = { laneId: 2, laneEpoch: 1 }
  f.attempts[0]!.finish({ type: 'admitted', lane: old })
  await settle()
  f.available(1)
  f.step(46_000); f.reply()
  await settle()
  expect(f.attempts[1]!.ordinal).toEqual({ peerSlot: f.attempts[0]!.ordinal.peerSlot, attemptSequence: 2n })
  expect(f.released).toEqual([old.laneId])
  await f.manager.reset()
})

it('drains admitted paths once across resets and resumes TURN on a subsequent content sample', async () => {
  const gate = deferred<void>()
  const f = fixture(lane => lane.laneId === 2 ? gate.promise : Promise.resolve())
  f.step(0); f.reply()
  f.attempts[0]!.finish({ type: 'admitted', lane: { laneId: 2, laneEpoch: 1 } })
  await settle()
  f.available(2)
  f.step(46_000); f.reply()
  f.attempts[1]!.finish({ type: 'admitted', lane: { laneId: 3, laneEpoch: 2 } })
  await settle()
  const resetting = f.manager.reset()
  expect(f.manager.reset()).toBe(resetting)
  const count = f.requests.length, admitted = f.traces.filter(t => t.stage === 'turn_path_admitted').length
  f.available(1)
  f.step(4000)
  expect(f.requests).toHaveLength(count)
  gate.resolve()
  await resetting
  expect(f.manager.settlingUntil).toBe(0)
  expect(f.traces.filter(t => t.stage === 'turn_path_admitted')).toHaveLength(admitted)
  expect(f.released).toEqual([2, 3])
  f.step(1000)
  expect(f.requests).toHaveLength(count + 1)
  expect(f.requests.at(-1)?.mode).toBe('parallel')
  await f.manager.reset()
})

it('holds the slot of an unpublished result through cancellation cleanup', async () => {
  const gate = deferred<void>()
  const f = fixture(() => gate.promise)
  f.step(0); f.reply()
  const resetting = f.manager.reset()
  f.attempts[0]!.finish({ type: 'admitted', lane: { laneId: 2, laneEpoch: 1 } })
  await settle()
  const otherSlots = Array.from({ length: 7 }, () => f.slots.acquire())
  expect(f.slots.tryAcquire()).toBeUndefined()
  expect(f.released).toEqual([2])
  gate.resolve()
  await resetting
  const freed = f.slots.acquire()
  expect(freed.next().peerSlot).toBe(f.attempts[0]!.ordinal.peerSlot)
  for (const lease of [...otherSlots, freed]) lease.close()
})

it('frees exhausted service places and slots for a new fallback while ignoring repeated dead service grants', async () => {
  const f = fixture()
  const failed = ['a', 'b', 'c', 'd']
  f.step(0); f.reply('ok', failed)
  for (const attempt of f.attempts) attempt.finish({ type: 'lifecycle-cancelled', owner: 'last-activation' })
  await settle()
  let usefulBytes = 0
  for (const delay of [2000, 10_000, 30_000]) {
    const count = f.requests.length
    usefulBytes += delay * 4096
    f.step(delay, { ...network, usefulBytes })
    const targeted = f.requests.slice(count)
    expect(targeted.map(r => r.serviceIds[0])).toEqual(failed)
    for (const request of targeted) f.reply('retry_later', [], 0n, request)
  }
  const leases = Array.from({ length: 8 }, () => f.slots.acquire())
  leases.forEach(lease => lease.close())
  f.step(3000, { ...network, usefulBytes })
  expect(f.requests.at(-1)).toMatchObject({ mode: 'all', serviceIds: [] })
  f.reply('ok', ['a', 'b', 'c', 'fallback'])
  expect(f.attempts).toHaveLength(5)
  await f.manager.reset()
})

it('correlates targeted refresh and detachment with the TURN lane and caps nonstatic grant TTL', async () => {
  const f = fixture()
  f.step(0); f.reply('ok', ['home'], 1n << 63n)
  const first = f.attempts[0]!, lane = { laneId: 2, laneEpoch: 1 }
  first.finish({ type: 'admitted', lane })
  await settle()
  f.step(86_400_000 - 30_000, { ...network, usefulBytes: 99_000 * MiB })
  expect(f.traces.find(t => t.stage === 'turn_demand_sent')).toEqual({ stage: 'turn_demand_sent' })
  const grantId = f.traces.find(t => t.stage === 'turn_path_admitted')?.grantId
  expect(grantId).toBeTruthy()
  expect(f.traces.at(-1)).toEqual({ stage: 'turn_refresh_requested', grantId, path: first.path, lane })
  f.reply('retry_later')
  expect(f.traces.at(-1)).toEqual({ stage: 'turn_grants_received', path: first.path, lane })
  f.manager.detached(first.path, lane)
  expect(f.traces.at(-1)).toEqual({ stage: 'turn_path_closed', reason: 'lane_lost', grantId, path: first.path, lane })
  await f.manager.reset()
})
