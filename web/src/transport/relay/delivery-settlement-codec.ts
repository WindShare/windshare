import { decodeDeliveryTotals, encodeDeliveryTotals, type RelayDeliveryTotals } from './delivery-totals-codec'
import { RelayProtocolError } from './protocol'

const NOTICE_MAGIC = 'WS2N'
const ACK_MAGIC = 'WS2S'

/** Ordered after all submitted WS2O frames once the relay has discarded unsent data. */
export interface RelaySettlementNotice extends RelayDeliveryTotals {
  readonly kind: 'settlement-notice'
}

/** Sent only after the destination consumes or discards the notice's entire prefix. */
export interface RelaySettlementAck extends RelayDeliveryTotals {
  readonly kind: 'settlement-ack'
}

export function encodeSettlementNotice(notice: RelaySettlementNotice): Uint8Array<ArrayBuffer> {
  if (notice.kind !== 'settlement-notice') {
    throw new RelayProtocolError('malformed', 'Expected a settlement notice')
  }
  return encodeDeliveryTotals(notice, NOTICE_MAGIC)
}

export function decodeSettlementNotice(encoded: Uint8Array): RelaySettlementNotice {
  return Object.freeze({ ...decodeDeliveryTotals(encoded, NOTICE_MAGIC), kind: 'settlement-notice' })
}

export function encodeSettlementAck(ack: RelaySettlementAck): Uint8Array<ArrayBuffer> {
  if (ack.kind !== 'settlement-ack') {
    throw new RelayProtocolError('malformed', 'Expected a settlement acknowledgement')
  }
  return encodeDeliveryTotals(ack, ACK_MAGIC)
}

export function decodeSettlementAck(encoded: Uint8Array): RelaySettlementAck {
  return Object.freeze({ ...decodeDeliveryTotals(encoded, ACK_MAGIC), kind: 'settlement-ack' })
}
