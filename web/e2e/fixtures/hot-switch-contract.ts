import type {
  V2PeerAttemptTraceEvent,
  V2PeerRecoveryTraceEvent,
} from '../../src/connectivity/diagnostics'
import type { V2PeerRecoveryPolicy } from '../../src/connectivity/peer-set/path'

export interface HotSwitchDispatch {
  readonly dispatchSequence: number
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: 'application-relay' | 'direct' | 'turn'
}

export interface HotSwitchLaneObservation {
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: 'application-relay' | 'direct' | 'turn'
}

export interface ObservedTransferFailure {
  readonly kind: 'directory' | 'file'
  readonly id: string
  readonly reason: string
}

export interface ObservedJobOutcome {
  readonly status: 'Succeeded' | 'CompletedWithErrors' | 'Paused' | 'Aborted' | 'NeedsAttention'
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

export interface HotSwitchRecoveryControl {
  readonly policy: V2PeerRecoveryPolicy
}

type WithoutCorrelation<Event> = Event extends unknown ? Omit<Event, 'correlation'> : never

type RecoveryBridgePayload<Event> = Event extends {
  readonly stage: 'attempt-replaced'
  readonly previousAttemptId: unknown
}
  ? Omit<Event, 'correlation' | 'previousAttemptId'> & Readonly<{
      previousAttemptIdBytes: readonly number[]
    }>
  : WithoutCorrelation<Event>

export type HotSwitchPeerAttemptEvidence = WithoutCorrelation<V2PeerAttemptTraceEvent> & Readonly<{
  protocolSessionIdBytes: readonly number[]
  peerPathIdBytes: readonly number[]
  attemptIdBytes: readonly number[]
  operationIdBytes?: readonly number[]
  lane?: Readonly<{ laneId: number; laneEpoch: number }>
}>

export type HotSwitchPeerRecoveryEvidence = RecoveryBridgePayload<V2PeerRecoveryTraceEvent> & Readonly<{
  protocolSessionIdBytes: readonly number[]
  peerPathIdBytes: readonly number[]
  attemptIdBytes?: readonly number[]
  lane?: Readonly<{ laneId: number; laneEpoch: number }>
}>

export interface HotSwitchAdmissionGateObservation {
  readonly offerOrdinal: number
  readonly release: 'attempt-timeout' | 'page-controlled'
}

/** Product observations cross the Playwright bridge without evidence-process ownership. */
export type HotSwitchPageEvent =
  | { readonly kind: 'attempt'; readonly evidence: HotSwitchPeerAttemptEvidence }
  | { readonly kind: 'recovery'; readonly evidence: HotSwitchPeerRecoveryEvidence }
  | {
      readonly kind: 'admission-response-gated'
      readonly observation: HotSwitchAdmissionGateObservation
    }
  | { readonly kind: 'dispatch'; readonly observation: HotSwitchDispatch }
  | { readonly kind: 'lane-admitted'; readonly observation: HotSwitchLaneObservation }
  | { readonly kind: 'lane-detached'; readonly observation: HotSwitchLaneObservation }
  | { readonly kind: 'relay-ineligible' }
  | ({ readonly kind: 'delivery' } & HotSwitchDeliveryTerminal)
  | ({ readonly kind: 'runtime-settled' } & HotSwitchRuntimeTerminal)
