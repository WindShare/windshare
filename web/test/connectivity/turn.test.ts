import { afterEach, expect, it, vi } from 'vitest'
import { encodeTurnControl, decodeTurnControl, turnRTCConfiguration, type TurnControl, type TurnGrant } from '../../src/connectivity/turn/control-codec'
import { TurnReceiver } from '../../src/connectivity/turn/receiver'
import { createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import { browserV2PeerRecoveryClock } from '../../src/connectivity/peer-set/contract'
import { PeerTabAdmission } from '../../src/connectivity/peer-set/tab-admission'
import { readFileSync } from 'node:fs'

const header = { sessionId: new Uint8Array(16).fill(1), ownerPathId: new Uint8Array(16).fill(2), sequence: 1n }
const grant: TurnGrant = { ...header, kind: 'grant', pathId: new Uint8Array(16).fill(3), replaces: new Uint8Array(16),
  provider: 'coturn', account: 'owner', profile: 'primary',
  credentials: { kind: 'expiring', username: 'user', password: 'password', validForMs: 60_000 },
  urls: ['turn:localhost:3478?transport=udp', 'turns:localhost:443?transport=tcp'] }

afterEach(() => vi.useRealTimers())

it('reconstructs every Go-authored TURN delegation message byte for byte', () => {
  const vectors = JSON.parse(readFileSync(new URL('../../../core/testvectors/turn-control-v4.json', import.meta.url), 'utf8')) as { bodyB64: string }[]
  for (const vector of vectors) {
    const body = Uint8Array.from(Buffer.from(vector.bodyB64, 'base64'))
    expect([...encodeTurnControl(decodeTurnControl(body))]).toEqual([...body])
  }
})

it('preserves bounded endpoint alternatives, expiry, authority and unsigned useful counts', () => {
  const messages: TurnControl[] = [grant, { ...header, kind: 'available', profiles: ['primary', 'secondary'] },
    { ...header, kind: 'demand', holdForMs: 120_000 }, { ...header, kind: 'ready', pathId: grant.pathId, laneId: 7, laneEpoch: 2 },
    { ...header, kind: 'retire', pathId: grant.pathId, reason: 2 },
    { ...header, kind: 'unavailable', profile: 'primary', retryAfterMs: 10_000, reason: 'unavailable' },
    { ...header, kind: 'report', pathId: grant.pathId, usefulBytes: 1n << 40n },
    { ...header, kind: 'recover', pathId: grant.pathId, reason: 1 },
    { ...header, kind: 'recover', pathId: grant.pathId, reason: 2 }]
  for (const message of messages) expect(decodeTurnControl(encodeTurnControl(message))).toEqual(message)
  expect(turnRTCConfiguration(grant)).toMatchObject({ iceTransportPolicy: 'relay', iceServers: [{ urls: grant.urls }] })
  expect(() => encodeTurnControl({ ...grant, replaces: grant.pathId })).toThrow()
  expect(() => encodeTurnControl({ ...header, kind: 'demand', holdForMs: 120_001 })).toThrow()
  for (const reason of [0, 3]) expect(() => encodeTurnControl({ ...header, kind: 'recover', pathId: grant.pathId, reason })).toThrow()
  expect(() => turnRTCConfiguration({ ...grant, urls: ['turn:[::1]:3478'] })).toThrow()
  expect(() => turnRTCConfiguration({ ...grant, urls: ['turns:localhost:443?transport=udp'] })).toThrow()
})

it('starts only for demand and replaces over surviving session control without retiring the old lane early', async () => {
  vi.useFakeTimers()
  let receive!: (body: Uint8Array<ArrayBuffer>) => void
  const sent: TurnControl[] = []
  const opened: TurnGrant[] = []
  const retired: Uint8Array[] = []
  const session = {
    protocolSessionIdentity: createV2ProtocolSessionIdentity(header.sessionId),
    subscribeTurnControls: (listener: typeof receive) => { receive = listener; return () => undefined },
    sendTurnControl: async (body: Uint8Array) => { sent.push(decodeTurnControl(body)) },
  } as unknown as V2ReceiverSessionRuntime
  const receiver = new TurnReceiver({ session, clock: browserV2PeerRecoveryClock, enabled: () => true,
    open: (value) => opened.push(value), retire: async (path) => { retired.push(path) } })
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['primary'] }))
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['unexpected'] }))
  expect(sent).toEqual([])
  receiver.setDemand(true)
  expect(sent[0]).toMatchObject({ kind: 'demand', holdForMs: 120_000 })
  receive(encodeTurnControl(grant))
  receiver.admitted(grant.pathId, { laneId: 7, laneEpoch: 2 })
  const replacement = { ...grant, pathId: new Uint8Array(16).fill(4), replaces: grant.pathId }
  receive(encodeTurnControl(replacement))
  receiver.admitted(replacement.pathId, { laneId: 8, laneEpoch: 3 })
  expect(opened).toHaveLength(2)
  expect(retired).toHaveLength(0)
  receive(encodeTurnControl({ ...header, kind: 'retire', pathId: grant.pathId, reason: 2 }))
  expect(retired).toEqual([grant.pathId])
  receive(encodeTurnControl(grant))
  expect(opened).toHaveLength(2)
  await vi.advanceTimersByTimeAsync(60_000)
  expect(retired).toEqual([grant.pathId, replacement.pathId])
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})

it('reserves tab attempt capacity for direct connectivity during TURN churn', async () => {
  const gate = new PeerTabAdmission({ concurrentAttempts: 2 })
  const abort = new AbortController()
  const turn = await gate.acquire({}, 0, 1000, abort.signal, 'turn')
  let extraAdmitted = false
  const waiting = gate.acquire({}, 0, 1000, abort.signal, 'turn').then((permit) => { extraAdmitted = true; return permit })
  const direct = await gate.acquire({}, 0, 1000, abort.signal, 'direct')
  expect(extraAdmitted).toBe(false)
  direct.release()
  expect(extraAdmitted).toBe(false)
  turn.release()
  const extra = await waiting
  extra.release()
  abort.abort()
})

it('retries readiness and useful reports across a temporary control-lane failure', async () => {
  vi.useFakeTimers()
  let receive!: (body: Uint8Array<ArrayBuffer>) => void
  const sent: TurnControl[] = []
  let readyAttempts = 0
  let reportAttempts = 0
  let usefulBytes = 1024
  const session = {
    protocolSessionIdentity: createV2ProtocolSessionIdentity(header.sessionId),
    subscribeTurnControls: (listener: typeof receive) => { receive = listener; return () => undefined },
    sendTurnControl: async (body: Uint8Array) => {
      const message = decodeTurnControl(body)
      if (message.kind === 'ready' && ++readyAttempts === 1) throw new Error('old lane closed')
      if (message.kind === 'report' && ++reportAttempts === 1) throw new Error('control lane changed')
      sent.push(message)
    },
  } as unknown as V2ReceiverSessionRuntime
  const receiver = new TurnReceiver({ session, clock: browserV2PeerRecoveryClock, enabled: () => true,
    open: () => undefined, retire: async () => undefined, measure: () => ({ usefulBytes, unhelpful: false }) })
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['primary'] }))
  receiver.setDemand(true)
  receive(encodeTurnControl(grant))
  receiver.admitted(grant.pathId, { laneId: 7, laneEpoch: 2 })
  await vi.advanceTimersByTimeAsync(2000)
  expect(readyAttempts).toBe(2)
  expect(reportAttempts).toBe(2)
  expect(sent).toContainEqual({ ...header, kind: 'report', pathId: grant.pathId, usefulBytes: 1024n })
  usefulBytes = 2048
  receiver.setDemand(false)
  expect(sent.slice(-2).map((message) => message.kind)).toEqual(['report', 'demand'])
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})
