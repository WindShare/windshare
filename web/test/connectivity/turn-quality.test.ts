import { afterEach, expect, it, vi } from 'vitest'
import { LanePerformance } from '../../src/content/scheduling/performance'
import { TurnReceiver, type TurnMeasurement } from '../../src/connectivity/turn/receiver'
import { decodeTurnControl, encodeTurnControl, type TurnControl, type TurnGrant } from '../../src/protocol/turn-control'
import { browserV2PeerRecoveryClock } from '../../src/connectivity/peer-set/contract'
import { createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'

afterEach(() => vi.useRealTimers())

it('measures overlapping content once and excludes output pauses', () => {
  const performance = new LanePerformance()
  performance.begin(0, 100)
  performance.begin(1000, 200)
  performance.complete(2000, 100, true)
  performance.complete(3000, 200, false)
  expect(performance.busyMilliseconds(60_000)).toBe(3000)
  performance.begin(60_000, 100)
  expect(performance.busyMilliseconds(61_000)).toBe(4000)
})

it('scopes quality to the network and lane while accounting survives lane replacement', async () => {
  vi.useFakeTimers()
  const header = { sessionId: new Uint8Array(16).fill(1), ownerPathId: new Uint8Array(16).fill(2), sequence: 1n }
  const grant: TurnGrant = { ...header, kind: 'grant', pathId: new Uint8Array(16).fill(3), replaces: new Uint8Array(16),
    provider: 'test', account: 'owner', profile: 'primary', credentials: { kind: 'fixed', username: 'user', password: 'password' },
    urls: ['turn:localhost?transport=tcp'] }
  let receive!: (body: Uint8Array<ArrayBuffer>) => void
  const sent: TurnControl[] = []
  const session = {
    protocolSessionIdentity: createV2ProtocolSessionIdentity(header.sessionId),
    subscribeTurnControls: (listener: typeof receive) => { receive = listener; return () => undefined },
    sendTurnControl: async (body: Uint8Array) => { sent.push(decodeTurnControl(body)) },
  } as unknown as V2ReceiverSessionRuntime
  let network = new Uint8Array(16).fill(4)
  let measurement: TurnMeasurement = { usefulBytes: 2048, unhelpful: false, busyMilliseconds: 1000, pendingBytes: 4096 }
  const receiver = new TurnReceiver({ session, clock: browserV2PeerRecoveryClock, enabled: () => true,
    open: () => undefined, retire: async () => undefined, measure: () => measurement, networkGeneration: () => network })
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['primary'] }))
  receiver.setDemand(true)
  receive(encodeTurnControl(grant))
  receiver.admitted(grant.pathId, { laneId: 7, laneEpoch: 1 })
  await vi.advanceTimersByTimeAsync(1000)
  const first = sent.find((message) => message.kind === 'quality')
  expect(first).toMatchObject({ kind: 'quality', laneId: 7, laneEpoch: 1, networkGeneration: network,
    sampleSequence: 1n, contentBytes: 2048n, busyMilliseconds: 1000n, pendingBytes: 4096n })
  network = new Uint8Array(16).fill(5)
  measurement = { usefulBytes: 512, unhelpful: false, busyMilliseconds: 500, pendingBytes: 1024 }
  receiver.admitted(grant.pathId, { laneId: 8, laneEpoch: 2 })
  await vi.advanceTimersByTimeAsync(1000)
  expect(sent.filter((message) => message.kind === 'quality').at(-1)).toMatchObject({ laneId: 8, laneEpoch: 2,
    networkGeneration: network, sampleSequence: 2n, contentBytes: 512n, busyMilliseconds: 500n })
  expect(sent.filter((message) => message.kind === 'report').at(-1)).toMatchObject({ usefulBytes: 2560n })
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})

it('rejects quality without a sample order or authenticated incarnation', () => {
  const quality: Extract<TurnControl, { kind: 'quality' }> = {
    kind: 'quality', sessionId: new Uint8Array(16).fill(1), ownerPathId: new Uint8Array(16).fill(2), sequence: 1n,
    pathId: new Uint8Array(16).fill(3), laneId: 1, laneEpoch: 2, networkGeneration: new Uint8Array(16),
    sampleSequence: 1n, contentBytes: 1n << 40n, busyMilliseconds: 1000n, pendingBytes: 1024n,
  }
  expect(decodeTurnControl(encodeTurnControl(quality))).toEqual(quality)
  for (const invalid of [{ ...quality, sampleSequence: 0n }, { ...quality, laneEpoch: 0 }, { ...quality, pendingBytes: 1n << 64n }]) {
    expect(() => encodeTurnControl(invalid)).toThrow()
  }
})
