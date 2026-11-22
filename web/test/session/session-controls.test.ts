import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { equalBytes } from '../../src/crypto/bytes'
import { decodePeerPathControl, encodePeerPathControl } from '../../src/protocol/peer-path-control'
import {
  decodeMessage, encodeMessage, MESSAGE_KIND, validateSenderControlBody,
  type MessageKind,
} from '../../src/session/message'
import { OperationRouter } from '../../src/session/operation-router'
import { openSent, runtimeFixture } from './v2-send-fixture'

interface ControlVector {
  readonly name: string
  readonly kind: MessageKind
  readonly bodyB64: string
  readonly plaintextB64: string
}

const { cases } = JSON.parse(readFileSync(new URL('../../../core/testvectors/v2-session-controls.json', import.meta.url), 'utf8')) as {
  readonly cases: readonly ControlVector[]
}
const bytes = (value: string) => Uint8Array.from(Buffer.from(value, 'base64'))

it.each(cases)('reconstructs the Go $name session message with its canonical body', (vector) => {
  const body = bytes(vector.bodyB64)
  const message = decodeMessage(bytes(vector.plaintextB64))
  expect(message.kind).toBe(vector.kind)
  expect(message.operationId).toBeUndefined()
  expect(equalBytes(encodeMessage(message.kind, undefined, message.body).plaintext, message.plaintext)).toBe(true)
  expect(() => encodeMessage(message.kind, 1n, message.body)).toThrow()
  validateSenderControlBody(message.kind, body)
  expect(equalBytes(encodePeerPathControl(decodePeerPathControl(body)), body)).toBe(true)
  expect(equalBytes(message.body, body)).toBe(true)
})

it('rejects another control family or an unsupported schema without changing decoders', () => {
  for (const vector of cases) {
    expect(() => validateSenderControlBody(MESSAGE_KIND.sessionTerminal, bytes(vector.bodyB64))).toThrow()
    const future = bytes(vector.bodyB64)
    future[1] = future[1]! + 1
    expect(() => validateSenderControlBody(vector.kind, future)).toThrow(/schema/i)
  }
})

it('sends peer path control on the existing session lane', async () => {
  const { runtime, channel } = runtimeFixture()
  const path = bytes(cases.find((vector) => vector.name === 'peer-path-demand')!.bodyB64)
  channel.unblock()
  try {
    await runtime.sendPeerPathControl(path)
    const sent = await openSent(channel)
    expect(sent.map(({ message }) => [message.kind, message.operationId, message.body])).toEqual([
      [MESSAGE_KIND.peerPathControl, undefined, path],
    ])
    const cancellation = new AbortController()
    cancellation.abort(new Error('demand withdrawn'))
    await expect(runtime.sendPeerPathControl(path, { signal: cancellation.signal })).rejects.toThrow('demand withdrawn')
    expect(channel.sent).toHaveLength(1)
  } finally {
    await runtime.close()
  }
  await expect(runtime.sendPeerPathControl(path)).rejects.toThrow()
})

it('isolates peer path subscriptions from operation lifetimes and termination', async () => {
  const router = new OperationRouter(() => undefined)
  const paths: Uint8Array[] = []
  const unsubscribePath = router.subscribePeerPathControls((body) => paths.push(body))
  const path = cases.find((vector) => vector.name === 'peer-path-mapping-ready')!
  const pathMessage = encodeMessage(path.kind, undefined, bytes(path.bodyB64))
  await router.route(pathMessage)
  expect(paths).toEqual([pathMessage.body])
  expect(router.active()).toEqual([])
  unsubscribePath()
  await router.route(pathMessage)
  expect(paths).toHaveLength(1)
  router.subscribePeerPathControls((body) => paths.push(body))
  router.terminate(new Error('session ended'))
  await router.route(pathMessage)
  expect(paths).toHaveLength(1)
})
