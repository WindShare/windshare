import type { V2BrowserConnectivityAttemptDiagnostic } from '../../src/connectivity/diagnostics'

export interface HotSwitchDispatch {
  readonly dispatchSequence: number
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: 'relay' | 'peer'
}

export interface HotSwitchLaneObservation {
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: 'relay' | 'peer'
}

export interface ObservedTransferFailure {
  readonly kind: 'directory' | 'file'
  readonly id: string
  readonly reason: string
}

export interface ObservedJobOutcome {
  readonly status: 'Succeeded' | 'CompletedWithErrors' | 'Aborted'
  readonly failures: readonly ObservedTransferFailure[]
  readonly failureCount: number
  readonly omittedFailureCount: number
}

export interface HotSwitchDeliveryEvidence {
  readonly expectedBytes: number
  readonly receivedBytes: number
  readonly expectedSha256: string
  readonly receivedSha256: string | null
  readonly terminal: 'succeeded' | 'failed'
}

export interface HotSwitchDeliveryTerminal {
  readonly outcome: 'succeeded' | 'failed'
  readonly evidence: HotSwitchDeliveryEvidence
  readonly jobOutcome?: ObservedJobOutcome
  readonly failureMessage?: string
}

export interface HotSwitchRuntimeTerminal {
  readonly error?: string
}

/** Product observations cross the Playwright bridge without evidence-process ownership. */
export type HotSwitchPageEvent =
  | { readonly kind: 'attempt'; readonly evidence: V2BrowserConnectivityAttemptDiagnostic }
  | { readonly kind: 'dispatch'; readonly observation: HotSwitchDispatch }
  | { readonly kind: 'lane-admitted'; readonly observation: HotSwitchLaneObservation }
  | { readonly kind: 'lane-detached'; readonly observation: HotSwitchLaneObservation }
  | { readonly kind: 'relay-ineligible' }
  | ({ readonly kind: 'delivery' } & HotSwitchDeliveryTerminal)
  | ({ readonly kind: 'runtime-settled' } & HotSwitchRuntimeTerminal)
