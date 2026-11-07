import { describe, expect, it } from 'vitest'
import { encode, rfc8949EncodeOptions } from 'cborg'
import { decodeMessage, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { decodeTurnControl, encodeTurnControl } from '../../src/protocol/turn-control'

const id = (value: number) => new Uint8Array(16).fill(value)
const wire = (value: unknown) => encode(value, rfc8949EncodeOptions)
const pair = '\u{16D63}\u{16D67}'

function expectRuntimePolicy(action: () => unknown, text: string): void {
  if (text.normalize('NFC') === text) expect(action).not.toThrow()
  else expect(action).toThrow(/NFC/)
}

describe('catalog NFC exceptions do not leak into session or TURN text', () => {
  const unavailable = { kind: 'unavailable' as const, sessionId: id(1), ownerPathId: id(2),
    sequence: 1n, profile: 'default', retryAfterMs: 0, reason: pair }
  const unavailableBody = [4, 5, id(1), id(2), 1, 'default', 0, pair]
  const grantBody = [4, 2, id(1), id(2), 1, id(3), new Uint8Array(16),
    'provider', 'account', 'profile', 1, pair, 'password', 0, ['turn:example.com']]

  it.each([
    ['session encode', () => encodeMessage(MESSAGE_KIND.listChildren, id(1), wire([pair]))],
    ['session decode', () => decodeMessage(wire(new Map<number, unknown>([
      [0, MESSAGE_KIND.listChildren], [1, id(1)], [2, [pair]],
    ])))],
    ['TURN username decode', () => decodeTurnControl(wire(grantBody))],
    ['TURN reason encode', () => encodeTurnControl(unavailable)],
    ['TURN reason decode', () => decodeTurnControl(wire(unavailableBody))],
    ['TURN session decode', () => decodeMessage(wire(new Map<number, unknown>([
      [0, MESSAGE_KIND.turnControl], [1, null], [2, unavailableBody],
    ])))],
  ] as const)('preserves the runtime NFC policy: %s', (_label, action) => {
    expectRuntimePolicy(action, pair)
  })

  it('preserves canonical bytes through TURN and session reencoding', () => {
    const reason = pair.normalize('NFC')
    const body = encodeTurnControl({ kind: 'unavailable', sessionId: id(1), ownerPathId: id(2),
      sequence: 1n, profile: 'default', retryAfterMs: 0, reason })
    const message = encodeMessage(MESSAGE_KIND.turnControl, undefined, body)
    const decoded = decodeMessage(message.plaintext)
    expect(decoded.body).toEqual(body)
    expect(encodeMessage(decoded.kind, decoded.operationId, decoded.body).plaintext).toEqual(message.plaintext)
    expect(decodeTurnControl(decoded.body)).toMatchObject({ reason })
  })
})
