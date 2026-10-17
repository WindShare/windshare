import type { RelayReceiveCapacity } from '../delivery-handshake-codec'

declare const sessionIdentity: unique symbol
declare const receiptIdentity: unique symbol

/** Local lifetime identities, never reconstructed from a wire ID or copied across connections. */
export interface ReceiveSession { readonly [sessionIdentity]: never }
export interface ReceiveReceipt { readonly [receiptIdentity]: never }

export interface ReceiveCounts {
  readonly frames: bigint
  readonly encodedBytes: bigint
}

export type ReceivePhase = 'active' | 'retiring' | 'draining' | 'settled'

export interface ReceiveProgress {
  readonly phase: ReceivePhase
  readonly received: ReceiveCounts
  readonly consumed: ReceiveCounts
  readonly discarded: ReceiveCounts
  readonly consumedPrefix: ReceiveCounts
}

export interface ReceiveUsage {
  readonly held: ReceiveCounts
  /** Includes locally completed data until its control is handed to the ordered writer. */
  readonly outstanding: ReceiveCounts
  readonly pendingConsumed: ReceiveCounts
  readonly retainedSessions: number
}

export type ReceiveAction =
  | 'open' | 'receive' | 'consume' | 'discard' | 'retire' | 'notice'
  | 'feedback' | 'settlement' | 'forget' | 'close'

export type ReceiveFailure =
  | 'config' | 'closed' | 'session' | 'session-limit' | 'capacity' | 'frame-size'
  | 'counter-exhausted' | 'receipt' | 'retirement' | 'frozen-prefix'
  | 'settlement' | 'settlement-pending'

export class ReceiveLedgerError extends Error {
  readonly code: ReceiveFailure

  constructor(code: ReceiveFailure) {
    super(`Receive ledger: ${code}`)
    this.name = 'ReceiveLedgerError'
    this.code = code
  }
}

export interface ReceiveLedgerEvent {
  readonly connectionId: string
  readonly sessionId: string | undefined
  readonly action: ReceiveAction
  readonly failure: ReceiveFailure | undefined
  readonly progress: ReceiveProgress | undefined
  readonly usage: ReceiveUsage
}

export interface ReceiveLedgerConfig {
  readonly connectionId: string
  readonly capacity: RelayReceiveCapacity
  readonly maxSessions: number
  readonly batch: RelayReceiveCapacity
  /** Synchronous diagnostics only; the observer must not throw or reenter the ledger. */
  readonly observe?: (event: ReceiveLedgerEvent) => void
}
