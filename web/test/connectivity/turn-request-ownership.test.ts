import { expect, it } from 'vitest'
import { TURNPaths } from '../../src/connectivity/turn-paths'
import { decodeTURNDemand, encodeTURNGrants, type TURNDemand, type TURNStatus } from '../../src/protocol/turn-control'
import { createPeerAttemptIdentity } from '../../src/session/identities'
import { PeerSlotPool } from '../../src/session/peer-slots/slots'

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
