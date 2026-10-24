import type { ReceiveFailure, ReceiveLedgerEvent } from '../../../contracts/relay-delivery-observation'
import type { RelayReceiveCapacity } from '../delivery-handshake-codec'

declare const sessionIdentity: unique symbol
declare const receiptIdentity: unique symbol

/** Local lifetime identities, never reconstructed from a wire ID or copied across connections. */
export interface ReceiveSession { readonly [sessionIdentity]: never }
export interface ReceiveReceipt { readonly [receiptIdentity]: never }

export class ReceiveLedgerError extends Error {
  readonly code: ReceiveFailure

  constructor(code: ReceiveFailure) {
    super(`Receive ledger: ${code}`)
    this.name = 'ReceiveLedgerError'
    this.code = code
  }
}

export interface ReceiveLedgerConfig {
  readonly connectionId: string
  readonly capacity: RelayReceiveCapacity
  readonly maxSessions: number
  /** Synchronous diagnostics only; the observer must not throw or reenter the ledger. */
  readonly observe?: (event: ReceiveLedgerEvent) => void
}
