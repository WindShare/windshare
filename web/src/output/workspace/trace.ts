import type {
  NeedsAttentionReason,
  ReceiveLifecycleState,
} from './state'

export type RecoveryDecision =
  | 'resume-receive'
  | 'resume-package'
  | 'restart-preparation'
  | 'waiting-to-save'
  | 'published'
  | 'download-started'
  | 'restart-required'
  | 'published-cleanup-retry'
  | 'expired'
  | 'needs-attention'

export type ReceiveOperationTraceEvent =
  | Readonly<{
      name: 'receive.operation.recovered'
      atMilliseconds: number
      context: Readonly<{
        operation_id: string
        observed_state: ReceiveLifecycleState['kind']
        reduced_state: ReceiveLifecycleState['kind']
        recovery_decision: RecoveryDecision
      }>
    }>
  | Readonly<{
      name: 'receive.operation.expired'
      atMilliseconds: number
      context: Readonly<{
        operation_id: string
        prior_stable_state: string
        expires_at_ms: number
      }>
    }>
  | Readonly<{
      name: 'receive.operation.needs_attention'
      atMilliseconds: number
      context: Readonly<{
        operation_id: string
        prior_state: ReceiveLifecycleState['kind']
        needs_attention_reason: NeedsAttentionReason
      }>
    }>

export type ReceiveOperationTraceListener = (event: ReceiveOperationTraceEvent) => void

export function observeRecovery(input: {
  readonly listener?: ReceiveOperationTraceListener
  readonly atMilliseconds: number
  readonly observed: ReceiveLifecycleState
  readonly reduced: ReceiveLifecycleState
  readonly decision: RecoveryDecision
}): void {
  input.listener?.(Object.freeze({
    name: 'receive.operation.recovered',
    atMilliseconds: input.atMilliseconds,
    context: Object.freeze({
      operation_id: input.observed.operationId,
      observed_state: input.observed.kind,
      reduced_state: input.reduced.kind,
      recovery_decision: input.decision,
    }),
  }))
  if (input.reduced.kind === 'expired') {
    input.listener?.(Object.freeze({
      name: 'receive.operation.expired',
      atMilliseconds: input.atMilliseconds,
      context: Object.freeze({
        operation_id: input.reduced.operationId,
        prior_stable_state: input.reduced.priorStableState,
        expires_at_ms: input.reduced.expiresAt,
      }),
    }))
  } else if (input.reduced.kind === 'needs-attention') {
    input.listener?.(Object.freeze({
      name: 'receive.operation.needs_attention',
      atMilliseconds: input.atMilliseconds,
      context: Object.freeze({
        operation_id: input.reduced.operationId,
        prior_state: input.observed.kind,
        needs_attention_reason: input.reduced.reason,
      }),
    }))
  }
}
