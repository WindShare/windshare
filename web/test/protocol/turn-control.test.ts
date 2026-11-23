import { encodeCanonicalCbor } from '../../src/protocol/cbor'
import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { decodeTURNDemand, decodeTURNGrants, encodeTURNDemand, encodeTURNGrants } from '../../src/protocol/turn-control'
import { MESSAGE_KIND, validateSenderControlBody } from '../../src/session/message'

const vector = JSON.parse(
  readFileSync(new URL('../../../core/testvectors/turn-control.json', import.meta.url), 'utf8'),
) as {
  cases: Array<{ sender: boolean; bodyB64: string }>
}
it('round trips the direction-bound Go demand and grant vectors', () => {
  for (const item of vector.cases) {
    const body = Uint8Array.from(Buffer.from(item.bodyB64, 'base64'))
    if (item.sender) {
      validateSenderControlBody(MESSAGE_KIND.turnControl, body)
      expect(encodeTURNGrants(decodeTURNGrants(body))).toEqual(body)
      expect(() => decodeTURNDemand(body)).toThrow()
    } else {
      expect(encodeTURNDemand(decodeTURNDemand(body))).toEqual(body)
      expect(() => validateSenderControlBody(MESSAGE_KIND.turnControl, body)).toThrow()
    }
  }
})

it('rejects duplicate identities, invalid ICE authorities, and oversized controls', () => {
  const id = new Uint8Array(16).fill(1)
  const row = (grantId: Uint8Array, url: string) => ['home', grantId, [url], 'user', 'password', 0]
  const grants = (...rows: unknown[][]) => [1, 2, 1, 1, false, rows]
  const valid = row(id, 'turn:localhost')
  for (const wire of [
    [1, 1, 1, 1, ['home', 'home']],
    [1, 3, 1, 1, []],
    grants(valid, valid),
    grants(row(new Uint8Array(16), 'turn:localhost')),
    ...['turn:[::::]', 'turns:bad_host:443', 'turn:u@host', 'turn:host:65536'].map((url) => grants(row(id, url))),
  ]) {
    const body = encodeCanonicalCbor(wire)
    expect(() => decodeTURNDemand(body)).toThrow()
    expect(() => decodeTURNGrants(body)).toThrow()
  }
  expect(() => decodeTURNGrants(new Uint8Array(16 * 1024 + 1))).toThrow()
})
