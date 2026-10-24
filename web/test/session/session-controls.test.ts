import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { equalBytes } from '../../src/crypto/bytes'
import { decodeTurnControl, encodeTurnControl } from '../../src/protocol/turn-control'
import { decodePeerPathControl, encodePeerPathControl } from '../../src/protocol/peer-path-control'
import {
  decodeMessage, encodeMessage, MESSAGE_KIND, validateSenderControlBody, verifySenderControl,
  type ControlBinding, type MessageKind,
} from '../../src/session/message'
import { OperationRouter } from '../../src/session/operation-router'
import { openSent, runtimeFixture } from './v2-send-fixture'

interface ControlVector {
  readonly name: string
  readonly kind: MessageKind
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
  const message = decodeMessage(bytes(vector.plaintextB64))
  expect(message.kind).toBe(vector.kind)
  expect(message.operationId).toBeUndefined()
  expect(equalBytes(encodeMessage(message.kind, undefined, message.body).plaintext, message.plaintext)).toBe(true)
  expect(() => encodeMessage(message.kind, new Uint8Array(16).fill(1), message.body)).toThrow()
  validateSenderControlBody(message.kind, body)
  if (message.kind === MESSAGE_KIND.turnControl) {
    expect(equalBytes(encodeTurnControl(decodeTurnControl(body)), body)).toBe(true)
  } else {
    expect(equalBytes(encodePeerPathControl(decodePeerPathControl(body)), body)).toBe(true)
  }
  if (vector.direction === 0) {
    expect(equalBytes(message.body, body)).toBe(true)
    return
  }
  const binding: ControlBinding = {
    shareInstance: bytes(vector.shareInstanceB64), protocolSessionId: bytes(vector.protocolSessionIdB64),
    laneId: vector.laneId, laneEpoch: vector.laneEpoch, direction: 1, sequence: BigInt(vector.sequence),
  }
  const verifier = createEd25519Verifier(bytes(vector.senderPublicKeyB64))
  expect(equalBytes(await verifySenderControl(message, binding, verifier), body)).toBe(true)
  const otherKind = message.kind === MESSAGE_KIND.turnControl ? MESSAGE_KIND.peerPathControl : MESSAGE_KIND.turnControl
  const substituted = encodeMessage(otherKind, undefined, message.body)
  await expect(verifySenderControl(substituted, binding, verifier)).rejects.toThrow(/signature/)
  const unsigned = encodeMessage(message.kind, undefined, body)
  await expect(verifySenderControl(unsigned, binding, verifier)).rejects.toThrow()
})

it('rejects another control family or an unsupported schema without changing decoders', () => {
  for (const vector of cases) {
    const otherKind = vector.kind === MESSAGE_KIND.turnControl ? MESSAGE_KIND.peerPathControl : MESSAGE_KIND.turnControl
    expect(() => validateSenderControlBody(otherKind, bytes(vector.bodyB64))).toThrow()
    const future = bytes(vector.bodyB64)
    future[1] = future[1]! + 1
    expect(() => validateSenderControlBody(vector.kind, future)).toThrow(/schema/i)
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
      [MESSAGE_KIND.peerPathControl, undefined, path],
      [MESSAGE_KIND.turnControl, undefined, turn],
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
  const router = new OperationRouter(() => undefined)
  const paths: Uint8Array[] = []
  const turns: Uint8Array[] = []
  const unsubscribePath = router.subscribePeerPathControls((body) => paths.push(body))
  const unsubscribeTurn = router.subscribeTurnControls((body) => turns.push(body))
  const path = cases.find((vector) => vector.name === 'peer-path-mapping-ready')!
  const turn = cases.find((vector) => vector.name === 'turn-grant')!
  const pathMessage = encodeMessage(path.kind, undefined, bytes(path.bodyB64))
  const turnMessage = encodeMessage(turn.kind, undefined, bytes(turn.bodyB64))
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
