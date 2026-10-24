import { afterEach, expect, it, vi } from 'vitest'
import { encodeTurnControl, decodeTurnControl, type TurnControl, type TurnGrant } from '../../src/protocol/turn-control'
import { TurnReceiver } from '../../src/connectivity/turn/receiver'
import { createProtocolSessionIdentity } from '../../src/session/identities'
import type { ReceiverSessionRuntime } from '../../src/session/runtime'
import { browserPeerRecoveryClock } from '../../src/connectivity/peer-set/contract'

afterEach(() => vi.useRealTimers())

it('replaces TURN inventory by revision while preserving demand and retiring withdrawn services', async () => {
  vi.useFakeTimers()
  const header = { sessionId: new Uint8Array(16).fill(1), ownerPathId: new Uint8Array(16).fill(2), sequence: 2n }
  let receive!: (body: Uint8Array<ArrayBuffer>) => void
  const sent: TurnControl[] = []
  const opened: TurnGrant[] = []
  const retired: Uint8Array[] = []
  const profilesChanged = vi.fn()
  const observe = vi.fn()
  const session = {
    protocolSessionIdentity: createProtocolSessionIdentity(header.sessionId),
    subscribeTurnControls: (listener: typeof receive) => { receive = listener; return () => undefined },
    sendTurnControl: async (body: Uint8Array) => { sent.push(decodeTurnControl(body)) },
  } as unknown as ReceiverSessionRuntime
  const receiver = new TurnReceiver({ session, clock: browserPeerRecoveryClock, enabled: () => true,
    open: (value) => opened.push(value), retire: async (path) => { retired.push(path) }, profilesChanged, observe })
  receiver.setDemand(true)
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['primary'] }))
  const first: TurnGrant = { ...header, sequence: 1n, kind: 'grant', pathId: new Uint8Array(16).fill(3),
    replaces: new Uint8Array(16), profile: 'primary', account: 'owner', provider: 'test',
    credentials: { kind: 'fixed', username: 'user', password: 'password' }, urls: ['turn:localhost'] }
  receive(encodeTurnControl(first))
  expect(opened).toEqual([first])
  receive(encodeTurnControl({ ...header, sequence: 3n, kind: 'available', profiles: ['secondary'] }))
  expect(retired).toEqual([first.pathId])
  receive(encodeTurnControl({ ...header, kind: 'available', profiles: ['primary'] }))
  receive(encodeTurnControl({ ...header, ownerPathId: new Uint8Array(16).fill(9), sequence: 9n,
    kind: 'available', profiles: ['primary'] }))
  expect(profilesChanged.mock.calls.map(([profiles]) => profiles)).toEqual([['primary'], ['secondary']])
  const second = { ...first, pathId: new Uint8Array(16).fill(4), profile: 'secondary' }
  receive(encodeTurnControl(second))
  expect(opened).toEqual([first, second])
  expect(sent.filter((message) => message.kind === 'demand')).toHaveLength(1)
  receive(encodeTurnControl({ ...header, sequence: 4n, kind: 'available', profiles: [] }))
  expect(retired).toEqual([first.pathId, second.pathId])
  expect(profilesChanged).toHaveBeenLastCalledWith([])
  expect(observe).toHaveBeenCalledWith({ profile: 'primary', state: 'withdrawn' })
  expect(observe).toHaveBeenCalledWith({ profile: 'secondary', state: 'withdrawn' })
  await receiver.close()
  expect(vi.getTimerCount()).toBe(0)
})
