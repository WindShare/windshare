import { describe, expect, it } from 'vitest'

import {
  decodeSettlementAck, decodeSettlementNotice, encodeSettlementAck, encodeSettlementNotice,
  type RelaySettlementAck, type RelaySettlementNotice,
} from '../../src/transport/relay/delivery-settlement-codec'
import type { RelayDeliveryTotals } from '../../src/transport/relay/delivery-totals-codec'
import { V2RelayProtocolError } from '../../src/transport/relay/v2-protocol'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface Vector extends VectorCase {
  readonly relaySessionIdB64: string
  readonly counts: readonly {
    name: string; totalFrames: string; totalEncodedBytes: string; encodedB64: string; accepted: boolean
  }[]
  readonly malformed: readonly { name: string; encodedB64: string }[]
}

const vectors = loadVectorFile(new URL('../../../core/testvectors/v2-session.json', import.meta.url))

for (const kind of ['settlement-notice', 'settlement-ack'] as const) {
  const vector = vectors.cases.find(candidate => candidate.name === `relay-${kind}`) as Vector
  const relaySessionId = b64ToBytes(vector.relaySessionIdB64)
  const encode = (totals: RelayDeliveryTotals) => kind === 'settlement-notice'
    ? encodeSettlementNotice({ ...totals, kind })
    : encodeSettlementAck({ ...totals, kind })
  const decode = kind === 'settlement-notice' ? decodeSettlementNotice : decodeSettlementAck

  describe(kind, () => {
    for (const test of vector.counts) {
      it(test.name, () => {
        const totals = {
          relaySessionId,
          totalFrames: BigInt(test.totalFrames),
          totalEncodedBytes: BigInt(test.totalEncodedBytes),
        }
        const encoded = b64ToBytes(test.encodedB64)
        if (test.accepted) {
          expect(encode(totals)).toEqual(encoded)
          expect(decode(encoded)).toEqual({ ...totals, kind })
        } else {
          expect(() => encode(totals)).toThrow(V2RelayProtocolError)
          expect(() => decode(encoded)).toThrow(V2RelayProtocolError)
        }
      })
    }
    for (const test of vector.malformed) {
      it(test.name, () => {
        expect(() => decode(b64ToBytes(test.encodedB64))).toThrow(V2RelayProtocolError)
      })
    }

    it('rejects values that DataView would coerce or truncate', () => {
      const totals = { relaySessionId, totalFrames: 1n, totalEncodedBytes: 21n }
      for (const value of [-1n, 1n << 64n, 1, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
        for (const field of ['totalFrames', 'totalEncodedBytes'] as const) {
          expect(() => encode({ ...totals, [field]: value } as RelayDeliveryTotals)).toThrow(V2RelayProtocolError)
        }
      }
      for (const id of [new Uint8Array(8), new Uint8Array(7).fill(1), new Uint8Array(9).fill(1)]) {
        expect(() => encode({ ...totals, relaySessionId: id })).toThrow(V2RelayProtocolError)
      }
    })

    it('owns the decoded identity even when the input is a bounded view', () => {
      const totals = { relaySessionId, totalFrames: 2n, totalEncodedBytes: 100n }
      const encoded = encode(totals)
      const padding = 3
      const storage = new Uint8Array(encoded.byteLength + 2 * padding)
      storage.set(encoded, padding)
      const decoded = decode(storage.subarray(padding, padding + encoded.byteLength))
      storage.fill(0)
      expect(decoded).toEqual({ ...totals, kind })
    })
  })
}

it('does not encode an acknowledgement as a notice or a notice as an acknowledgement', () => {
  const totals = { relaySessionId: new Uint8Array(8).fill(1), totalFrames: 0n, totalEncodedBytes: 0n }
  expect(() => encodeSettlementNotice({ ...totals, kind: 'settlement-ack' } as unknown as RelaySettlementNotice))
    .toThrow(V2RelayProtocolError)
  expect(() => encodeSettlementAck({ ...totals, kind: 'settlement-notice' } as unknown as RelaySettlementAck))
    .toThrow(V2RelayProtocolError)
})
