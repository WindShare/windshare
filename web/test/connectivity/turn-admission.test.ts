import { afterEach, expect, it, vi } from 'vitest'
import { decodeTurnControl, encodeTurnControl, type TurnControl, type TurnGrant } from '../../src/connectivity/turn/control-codec'
import { TurnReceiver } from '../../src/connectivity/turn/receiver'
import { browserV2PeerRecoveryClock } from '../../src/connectivity/peer-set/contract'
import { createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'

const header = { sessionId: new Uint8Array(16).fill(1), ownerPathId: new Uint8Array(16).fill(2), sequence: 1n }
const grant: TurnGrant = { ...header, kind: 'grant', pathId: new Uint8Array(16).fill(3), replaces: new Uint8Array(16),
  provider: 'test', account: 'owner', profile: 'primary',
  credentials: { kind: 'fixed', username: 'user', password: 'password' }, urls: ['turn:localhost'] }
const firstLane = { laneId: 7, laneEpoch: 1 }
const nextLane = { laneId: 7, laneEpoch: 2 }

afterEach(() => vi.useRealTimers())

function fixture(send: (message: TurnControl) => Promise<void>) {
  vi.useFakeTimers()
  let receive!: (body: Uint8Array<ArrayBuffer>) => void
  const sent: TurnControl[] = []
  const session = {
    protocolSessionIdentity: createV2ProtocolSessionIdentity(header.sessionId),
    subscribeTurnControls: (listener: typeof receive) => { receive = listener; return () => undefined },
    sendTurnControl: async (body: Uint8Array) => {
      const message = decodeTurnControl(body)
      sent.push(message)
      await send(message)
    },
  } as unknown as V2ReceiverSessionRuntime
  const receiver = new TurnReceiver({ session, clock: browserV2PeerRecoveryClock, enabled: () => true,
    open: () => undefined, retire: async () => undefined })
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['primary'] }))
  receiver.setDemand(true)
  receive(encodeTurnControl(grant))
  return { receiver, sent }
}

it('stops readiness retries for a detached lane and announces the next incarnation', async () => {
  const { receiver, sent } = fixture(async message => {
    if (message.kind === 'ready') throw new Error('control lane temporarily unavailable')
  })
  receiver.admitted(grant.pathId, firstLane)
  await vi.advanceTimersByTimeAsync(0)
  receiver.detached(grant.pathId, firstLane)
  await vi.advanceTimersByTimeAsync(2000)
  expect(sent.filter(message => message.kind === 'ready')).toHaveLength(1)
  receiver.admitted(grant.pathId, nextLane)
  receiver.detached(grant.pathId, firstLane)
  receiver.admitted(grant.pathId, firstLane)
  await vi.advanceTimersByTimeAsync(1000)
  const ready = sent.filter(message => message.kind === 'ready')
  expect(ready.map(message => message.laneEpoch)).toEqual([1, 2, 2])
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})

it('does not let late send completion suppress readiness retries for a new lane', async () => {
  let finishOld!: () => void
  const oldSend = new Promise<void>(resolve => { finishOld = resolve })
  let nextAttempts = 0
  const { receiver, sent } = fixture(async message => {
    if (message.kind !== 'ready') return
    if (message.laneEpoch === firstLane.laneEpoch) await oldSend
    else if (++nextAttempts === 1) throw new Error('retry on another control lane')
  })
  receiver.admitted(grant.pathId, firstLane)
  receiver.detached(grant.pathId, firstLane)
  receiver.admitted(grant.pathId, nextLane)
  finishOld()
  await vi.advanceTimersByTimeAsync(2000)
  expect(sent.filter(message => message.kind === 'ready').map(message => message.laneEpoch)).toEqual([1, 2, 2])
  expect(nextAttempts).toBe(2)
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})

it('rejects readiness without a bounded nonzero lane incarnation', () => {
  for (const lane of [
    { laneId: 0, laneEpoch: 1 },
    { laneId: 1, laneEpoch: 0 },
    { laneId: 2 ** 32, laneEpoch: 1 },
    { laneId: 1, laneEpoch: 2 ** 32 },
  ]) {
    expect(() => encodeTurnControl({ ...header, kind: 'ready', pathId: grant.pathId, ...lane })).toThrow()
  }
})
