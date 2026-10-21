import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { equalBytes } from '../../src/crypto/bytes'
import { decodeTurnControl, encodeTurnControl } from '../../src/connectivity/turn/control-codec'
import { decodeV2PeerPathControl, encodeV2PeerPathControl } from '../../src/connectivity/v2-path-control-codec'
import {
  decodeV2Message, encodeV2Message, V2_MESSAGE_KIND, validateV2SenderControlBody, verifyV2SenderControl,
  type V2ControlBinding, type V2MessageKind,
} from '../../src/session/v2-message'
import { V2OperationRouter } from '../../src/session/v2-operation-router'
import { openSent, runtimeFixture } from './v2-send-fixture'

interface ControlVector {
  readonly name: string
  readonly kind: V2MessageKind
  readonly direction: number
  readonly bodyB64: string
  readonly plaintextB64: string
  readonly senderPublicKeyB64: string
  readonly shareInstanceB64: string
  readonly protocolSessionIdB64: string
  readonly laneId: number
  readonly laneEpoch: number
  readonly sequence: string
}

const { cases } = JSON.parse(readFileSync(new URL('../../../core/testvectors/v2-session-controls.json', import.meta.url), 'utf8')) as {
  readonly cases: readonly ControlVector[]
}
const bytes = (value: string) => Uint8Array.from(Buffer.from(value, 'base64'))

it.each(cases)('reconstructs the Go $name session message and verifies its authority', async (vector) => {
  const body = bytes(vector.bodyB64)
  const message = decodeV2Message(bytes(vector.plaintextB64))
  expect(message.kind).toBe(vector.kind)
  expect(message.operationId).toBeUndefined()
  expect(equalBytes(encodeV2Message(message.kind, undefined, message.body).plaintext, message.plaintext)).toBe(true)
  expect(() => encodeV2Message(message.kind, new Uint8Array(16).fill(1), message.body)).toThrow()
  validateV2SenderControlBody(message.kind, body)
  if (message.kind === V2_MESSAGE_KIND.turnControl) {
    expect(equalBytes(encodeTurnControl(decodeTurnControl(body)), body)).toBe(true)
  } else {
    expect(equalBytes(encodeV2PeerPathControl(decodeV2PeerPathControl(body)), body)).toBe(true)
  }
  if (vector.direction === 0) {
    expect(equalBytes(message.body, body)).toBe(true)
    return
  }
  const binding: V2ControlBinding = {
    shareInstance: bytes(vector.shareInstanceB64), protocolSessionId: bytes(vector.protocolSessionIdB64),
    laneId: vector.laneId, laneEpoch: vector.laneEpoch, direction: 1, sequence: BigInt(vector.sequence),
  }
  const verifier = createEd25519Verifier(bytes(vector.senderPublicKeyB64))
  expect(equalBytes(await verifyV2SenderControl(message, binding, verifier), body)).toBe(true)
  const otherKind = message.kind === V2_MESSAGE_KIND.turnControl ? V2_MESSAGE_KIND.peerPathControl : V2_MESSAGE_KIND.turnControl
  const substituted = encodeV2Message(otherKind, undefined, message.body)
  await expect(verifyV2SenderControl(substituted, binding, verifier)).rejects.toThrow(/signature/)
  const unsigned = encodeV2Message(message.kind, undefined, body)
  await expect(verifyV2SenderControl(unsigned, binding, verifier)).rejects.toThrow()
})

it('rejects another control family or an unsupported schema without changing decoders', () => {
  for (const vector of cases) {
    const otherKind = vector.kind === V2_MESSAGE_KIND.turnControl ? V2_MESSAGE_KIND.peerPathControl : V2_MESSAGE_KIND.turnControl
    expect(() => validateV2SenderControlBody(otherKind, bytes(vector.bodyB64))).toThrow()
    const future = bytes(vector.bodyB64)
    future[1] = future[1]! + 1
    expect(() => validateV2SenderControlBody(vector.kind, future)).toThrow(/schema/i)
  }
})

it('sends each control through its own outer kind on the existing session lane', async () => {
  const { runtime, channel } = runtimeFixture()
  const path = bytes(cases.find((vector) => vector.name === 'peer-path-demand')!.bodyB64)
  const demand = decodeTurnControl(bytes(cases.find((vector) => vector.name === 'turn-demand')!.bodyB64))
  const turn = encodeTurnControl({ ...demand, sessionId: runtime.protocolSessionIdentity.copyBytes() })
  channel.unblock()
  try {
    await runtime.sendPeerPathControl(path)
    await runtime.sendTurnControl(turn)
    const sent = await openSent(channel)
    expect(sent.map(({ message }) => [message.kind, message.operationId, message.body])).toEqual([
      [V2_MESSAGE_KIND.peerPathControl, undefined, path],
      [V2_MESSAGE_KIND.turnControl, undefined, turn],
    ])
    await expect(runtime.sendPeerPathControl(turn)).rejects.toThrow()
    await expect(runtime.sendTurnControl(path)).rejects.toThrow()
    const cancellation = new AbortController()
    cancellation.abort(new Error('demand withdrawn'))
    await expect(runtime.sendTurnControl(turn, { signal: cancellation.signal })).rejects.toThrow('demand withdrawn')
    expect(channel.sent).toHaveLength(2)
  } finally {
    await runtime.close()
  }
  await expect(runtime.sendTurnControl(turn)).rejects.toThrow()
})

it('isolates subscriptions and keeps TURN independent of path handler and operation lifetimes', async () => {
  const router = new V2OperationRouter(() => undefined)
  const paths: Uint8Array[] = []
  const turns: Uint8Array[] = []
  const unsubscribePath = router.subscribePeerPathControls((body) => paths.push(body))
  const unsubscribeTurn = router.subscribeTurnControls((body) => turns.push(body))
  const path = cases.find((vector) => vector.name === 'peer-path-mapping-ready')!
  const turn = cases.find((vector) => vector.name === 'turn-grant')!
  const pathMessage = encodeV2Message(path.kind, undefined, bytes(path.bodyB64))
  const turnMessage = encodeV2Message(turn.kind, undefined, bytes(turn.bodyB64))
  await router.route(pathMessage)
  await router.route(turnMessage)
  expect(paths).toEqual([pathMessage.body])
  expect(turns).toEqual([turnMessage.body])
  expect(router.active()).toEqual([])
  unsubscribePath()
  await router.route(pathMessage)
  await router.route(turnMessage)
  expect(paths).toHaveLength(1)
  expect(turns).toHaveLength(2)
  unsubscribeTurn()
  await router.route(turnMessage)
  expect(turns).toHaveLength(2)
  router.subscribeTurnControls((body) => turns.push(body))
  router.terminate(new Error('session ended'))
  await router.route(pathMessage)
  await router.route(turnMessage)
  expect(paths).toHaveLength(1)
  expect(turns).toHaveLength(2)
})
