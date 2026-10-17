import { decodeDeliveryTotals, encodeDeliveryTotals, type RelayDeliveryTotals } from './delivery-totals-codec'

const MAGIC = 'WS2B'

/** Cumulative feedback for the coordinated cutover; not the live receive-credit codec. */
export type RelayDeliveryConsumed = RelayDeliveryTotals

export function encodeDeliveryConsumed(feedback: RelayDeliveryConsumed): Uint8Array<ArrayBuffer> {
  return encodeDeliveryTotals(feedback, MAGIC)
}

export function decodeDeliveryConsumed(encoded: Uint8Array): RelayDeliveryConsumed {
  return decodeDeliveryTotals(encoded, MAGIC)
}
