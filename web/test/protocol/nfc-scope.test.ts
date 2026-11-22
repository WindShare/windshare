import { describe, expect, it } from 'vitest'
import { encode, rfc8949EncodeOptions } from 'cborg'
import { decodeMessage, encodeMessage, MESSAGE_KIND, validateSenderControlBody } from '../../src/session/message'

const id = (value: number) => new Uint8Array(16).fill(value)
const wire = (value: unknown) => encode(value, rfc8949EncodeOptions)
const pair = '\u{16D63}\u{16D67}'

function expectRuntimePolicy(action: () => unknown, text: string): void {
  if (text.normalize('NFC') === text) expect(action).not.toThrow()
  else expect(action).toThrow(/NFC/)
}

describe('catalog NFC exceptions do not leak into session text', () => {
  const terminalBody = new Map<number, unknown>([[0, 1], [1, 0x1001], [2, pair]])

  it.each([
    ['session encode', () => encodeMessage(MESSAGE_KIND.listChildren, id(1), wire([pair]))],
    ['session decode', () => decodeMessage(wire(new Map<number, unknown>([
      [0, MESSAGE_KIND.listChildren], [1, id(1)], [2, [pair]],
    ])))],
    ['session terminal decode', () => validateSenderControlBody(MESSAGE_KIND.sessionTerminal, wire(terminalBody))],
  ] as const)('preserves the runtime NFC policy: %s', (_label, action) => {
    expectRuntimePolicy(action, pair)
  })

  it('preserves canonical bytes through session reencoding', () => {
    const reason = pair.normalize('NFC')
    const body = wire(new Map<number, unknown>([[0, 1], [1, 0x1001], [2, reason]]))
    const message = encodeMessage(MESSAGE_KIND.sessionTerminal, undefined, body)
    const decoded = decodeMessage(message.plaintext)
    expect(decoded.body).toEqual(body)
    expect(encodeMessage(decoded.kind, decoded.operationId, decoded.body).plaintext).toEqual(message.plaintext)
    validateSenderControlBody(decoded.kind, decoded.body)
  })
})
