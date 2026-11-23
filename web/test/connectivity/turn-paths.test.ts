import { expect, it } from 'vitest'
import { TURNPaths } from '../../src/connectivity/turn-paths'
import type { PeerAttemptResult } from '../../src/connectivity/peer-failure'
import { createPeerAttemptIdentity } from '../../src/session/identities'
import { PeerSlotPool, type PeerAttemptOrdinal } from '../../src/session/peer-slots/slots'
import { decodeTURNDemand, encodeTURNGrants, type TURNDemand, type TURNStatus } from '../../src/protocol/turn-control'

const network = {
  queuedBytes: 1,
  queuedBlocks: 1,
  networkWaitingBytes: 1,
  capacityWaitingBytes: 0,
  unfinishedBytes: 0,
  inFlightBytes: 0,
  remainingBytes: 8 * 1024 * 1024,
  remainingKnown: true,
  usefulBytes: 0,
  outputBytes: 0,
}
function fixture() {
  let now = 1000,
    activeLanes = [1],
    value = 0
  let listener: ((body: Uint8Array<ArrayBuffer>) => void) | undefined
  const requests: TURNDemand[] = []
  const attempts: Array<{ ordinal: PeerAttemptOrdinal; path: string; finish: (result: PeerAttemptResult) => void }> = []
  const released: number[] = []
  const manager = new TURNPaths({
    now: () => now,
    slots: new PeerSlotPool(),
    identity: () => new Uint8Array(16).fill(++value),
    session: {
      subscribeTURNControls: (cb) => {
        listener = cb
        return () => {
          listener = undefined
        }
      },
      sendTURNControl: async (b) => {
        requests.push(decodeTURNDemand(b))
      },
      availableLaneIds: () => activeLanes,
    },
    start: (_grant, path, ordinal) => {
      let resolve!: (result: PeerAttemptResult) => void
      const promise = new Promise<PeerAttemptResult>((finish) => {
        resolve = finish
      })
      attempts.push({ ordinal, path: path.copyBytes().toString(), finish: resolve })
      return {
        attemptId: createPeerAttemptIdentity(new Uint8Array(16).fill(3)),
        result: promise,
        cancel: (owner) => resolve({ type: 'lifecycle-cancelled', owner }),
      }
    },
    release: async (lane) => {
      released.push(lane.laneId)
    },
  })
  return {
    manager,
    attempts,
    requests,
    released,
    step: (elapsed: number, d = network) => {
      now += elapsed
      manager.step(d)
    },
    available: (...ids: number[]) => {
      activeLanes = ids
    },
    reply: (status: TURNStatus = 'ok') =>
      listener!(
        encodeTURNGrants({
          requestId: requests.at(-1)!.requestId,
          status,
          hasFallback: false,
          grants:
            status === 'ok'
              ? [
                  {
                    serviceId: 'home',
                    grantId: new Uint8Array(16).fill(++value),
                    urls: ['turn:localhost'],
                    username: 'u',
                    password: 'p',
                    validForMs: 60_000n,
                  },
                ]
              : [],
        }),
      ),
  }
}
const settle = () => new Promise<void>((resolve) => queueMicrotask(() => queueMicrotask(() => queueMicrotask(resolve))))
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
  f.step(60_000)
  expect(f.requests.map((d) => d.mode)).toEqual(['parallel', 'parallel', 'all', 'all'])
})
