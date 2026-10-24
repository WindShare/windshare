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

export interface ReceiveLedgerEvent {
  readonly connectionId: string
  readonly capacity: ReceiveCounts
  readonly sessionId: string | undefined
  readonly action: ReceiveAction
  readonly failure: ReceiveFailure | undefined
  readonly progress: ReceiveProgress | undefined
  readonly usage: ReceiveUsage
}
