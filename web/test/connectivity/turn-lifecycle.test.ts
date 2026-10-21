import { afterEach, expect, it, vi } from 'vitest'
import { encodeTurnControl, type TurnGrant } from '../../src/connectivity/turn/control-codec'
import { TurnReceiver, type TurnObservation } from '../../src/connectivity/turn/receiver'
import { browserV2PeerRecoveryClock } from '../../src/connectivity/peer-set/contract'
import { createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'

const header = { sessionId: new Uint8Array(16).fill(1), ownerPathId: new Uint8Array(16).fill(2), sequence: 1n }
const grant: TurnGrant = { ...header, kind: 'grant', pathId: new Uint8Array(16).fill(3), replaces: new Uint8Array(16),
  provider: 'cloudflare', account: 'owner', profile: 'primary',
  credentials: { kind: 'expiring', username: 'user', password: 'password', validForMs: 60_000 },
  urls: ['turn:localhost:3478?transport=udp'] }

afterEach(() => vi.useRealTimers())

function fixture(open: (receiver: TurnReceiver) => void = () => undefined) {
  vi.useFakeTimers()
  let receive!: (body: Uint8Array<ArrayBuffer>) => void
  const observations: TurnObservation[] = []
  const retire = vi.fn(async () => undefined)
  const session = {
    protocolSessionIdentity: createV2ProtocolSessionIdentity(header.sessionId),
    subscribeTurnControls: (listener: typeof receive) => { receive = listener; return () => undefined },
    sendTurnControl: async () => undefined,
  } as unknown as V2ReceiverSessionRuntime
  const receiver: TurnReceiver = new TurnReceiver({ session, clock: browserV2PeerRecoveryClock, enabled: () => true,
    open: () => open(receiver), retire, observe: event => observations.push(event) })
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['primary'] }))
  receiver.setDemand(true)
  receive(encodeTurnControl(grant))
  return { receiver, receive, retire, observations }
}

it('does not overwrite synchronous admission with connecting', async () => {
  const { receiver, observations } = fixture(receiver => receiver.admitted(grant.pathId))
  expect(observations.map(event => event.state)).toEqual(['available', 'connecting', 'ready'])
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})

it('keeps a synchronous budget failure retired and releases its path exactly once', async () => {
  const { receiver, retire, observations } = fixture(receiver => {
    receiver.recoveryChanged(grant.pathId, { kind: 'session-exhausted', reason: 'session-attempt-budget' })
    receiver.unhelpful(grant.pathId)
  })
  expect(observations.at(-1)).toMatchObject({ state: 'retired', reason: 'session-attempt-budget' })
  expect(receiver.grantFor(grant.pathId)).toBeUndefined()
  await vi.advanceTimersByTimeAsync(65_000)
  expect(retire).toHaveBeenCalledTimes(1)
  expect(observations.at(-1)).toMatchObject({ state: 'retired', reason: 'session-attempt-budget' })
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})

it('preserves opening failures through cleanup', async () => {
  const { receiver, observations } = fixture(() => { throw new Error('TURN setup refused') })
  expect(observations.at(-1)).toMatchObject({ state: 'retired', reason: 'Error: TURN setup refused' })
  await receiver.close()
})

it('publishes retry failures, clears them for a fresh attempt, and ignores retired path callbacks', async () => {
  const { receiver, observations, receive } = fixture()
  receiver.recoveryChanged(grant.pathId, { kind: 'waiting-retry', waveOrdinal: 1, retryOrdinal: 0,
    delayMilliseconds: 1000, reason: 'negotiation-timeout' })
  expect(observations.at(-1)).toMatchObject({ state: 'retrying', reason: 'negotiation-timeout' })
  receiver.recoveryChanged(grant.pathId, { kind: 'attempting', phase: 'negotiation', waveOrdinal: 1,
    waveAttemptOrdinal: 2, sessionAttemptOrdinal: 2 })
  expect(observations.at(-1)?.state).toBe('connecting')
  expect(observations.at(-1)?.reason).toBeUndefined()
  receive(encodeTurnControl({ ...header, kind: 'retire', pathId: grant.pathId, reason: 3 }))
  expect(observations.at(-1)).toMatchObject({ state: 'retired', reason: 'expired' })
  const count = observations.length
  receiver.recoveryChanged(grant.pathId, { kind: 'admitted', lane: { laneId: 7, laneEpoch: 1 } })
  receiver.admitted(grant.pathId)
  expect(observations).toHaveLength(count)
  await receiver.close()
})
