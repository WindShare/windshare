import { describe, expect, it } from 'vitest'

import {
  decodeDeliveryConsumed, encodeDeliveryConsumed, type RelayDeliveryConsumed,
} from '../../src/transport/relay/delivery-consumed-codec'
import { decodeReceiveCredit } from '../../src/transport/relay/receive-credit-codec'
import { V2RelayProtocolError } from '../../src/transport/relay/v2-protocol'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface CountCase {
  readonly name: string
  readonly totalFrames: string
  readonly totalEncodedBytes: string
  readonly encodedB64: string
  readonly accepted: boolean
}

interface Vector extends VectorCase {
  readonly relaySessionIdB64: string
  readonly counts: readonly CountCase[]
  readonly malformed: readonly { name: string; encodedB64: string }[]
}

const vector = loadVectorFile(
  new URL('../../../core/testvectors/v2-session.json', import.meta.url),
).cases.find(candidate => candidate.name === 'relay-delivery-consumed') as Vector

const relaySessionId = b64ToBytes(vector.relaySessionIdB64)

describe('cumulative relay delivery feedback', () => {
  for (const test of vector.counts) {
    it(test.name, () => {
      const feedback = {
        relaySessionId,
        totalFrames: BigInt(test.totalFrames),
        totalEncodedBytes: BigInt(test.totalEncodedBytes),
      }
      const encoded = b64ToBytes(test.encodedB64)
      if (test.accepted) {
        expect(encodeDeliveryConsumed(feedback)).toEqual(encoded)
        expect(decodeDeliveryConsumed(encoded)).toEqual(feedback)
        expect(() => decodeReceiveCredit(encoded)).toThrow(V2RelayProtocolError)
      } else {
        expect(() => encodeDeliveryConsumed(feedback)).toThrow(V2RelayProtocolError)
        expect(() => decodeDeliveryConsumed(encoded)).toThrow(V2RelayProtocolError)
      }
    })
  }

  for (const test of vector.malformed) {
    it(test.name, () => {
      expect(() => decodeDeliveryConsumed(b64ToBytes(test.encodedB64))).toThrow(V2RelayProtocolError)
    })
  }

  it('rejects out-of-domain inputs before a DataView can truncate them', () => {
    const valid = { relaySessionId, totalFrames: 1n, totalEncodedBytes: 21n }
    for (const value of [-1n, 1n << 64n, 1, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
      for (const field of ['totalFrames', 'totalEncodedBytes'] as const) {
        const malformed = { ...valid, [field]: value } as RelayDeliveryConsumed
        expect(() => encodeDeliveryConsumed(malformed)).toThrow(V2RelayProtocolError)
      }
    }
    for (const id of [new Uint8Array(8), new Uint8Array(7).fill(1), new Uint8Array(9).fill(1)]) {
      expect(() => encodeDeliveryConsumed({ ...valid, relaySessionId: id })).toThrow(V2RelayProtocolError)
    }
  })

  it('decodes a bounded view and retains an independent session identity', () => {
    const feedback = { relaySessionId, totalFrames: 2n, totalEncodedBytes: 100n }
    const encoded = encodeDeliveryConsumed(feedback)
    const padding = 3
    const storage = new Uint8Array(encoded.byteLength + 2 * padding)
    storage.set(encoded, padding)
    const decoded = decodeDeliveryConsumed(storage.subarray(padding, padding + encoded.byteLength))
    storage.fill(0)
    expect(decoded).toEqual(feedback)
  })
})
