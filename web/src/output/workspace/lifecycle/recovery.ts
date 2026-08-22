import {
  nextReceiveLifecycleState,
  stableDeadline,
  type PlanKind,
  type ReceiveLifecycleState,
} from '../state'
import type { LifecycleEvent, LifecycleReducerContext } from './events'
import { requireState } from './transitions'

export function pauseDirectZip(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'direct-zip-pause-verified' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'receiving')
  if (context.planKind !== 'direct-resumable-zip') {
    throw new TypeError('Direct ZIP checkpoint event requires a DirectResumableZip plan')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'resumable-receive',
    payloadKind: 'direct-zip',
    directZipCheckpointDigest: event.checkpointDigest,
    safeSelectedPayloadBytes: event.safeSelectedPayloadBytes,
    committedArchiveLength: event.committedArchiveLength,
    checkpointPhase: event.checkpointPhase,
    expiresAt: stableDeadline(context.nowMilliseconds),
  })
}

export function gateDirectZipRecovery(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'direct-zip-recovery-gated' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireDirectZipRecoveryState(state, context.planKind)
  return nextReceiveLifecycleState(state, {
    kind: event.gateKind,
    recoveryGateDigest: event.recoveryGateDigest,
    expiresAt: stableDeadline(context.nowMilliseconds),
  })
}

export function resumeDirectZipRecovery(
  state: ReceiveLifecycleState,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  if (context.planKind !== 'direct-resumable-zip' ||
      (state.kind !== 'authorization-required' &&
       state.kind !== 'target-verification-required' &&
       state.kind !== 'destination-space-required')) {
    throw new TypeError('Direct ZIP recovery resume requires a retained recovery gate')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'receiving',
    activeLeaseId: context.activeLeaseId,
  })
}

function requireDirectZipRecoveryState(
  state: ReceiveLifecycleState,
  planKind: PlanKind,
): void {
  if (planKind !== 'direct-resumable-zip' ||
      (state.kind !== 'receiving' && state.kind !== 'resumable-receive' &&
       state.kind !== 'authorization-required' &&
       state.kind !== 'target-verification-required' &&
       state.kind !== 'destination-space-required')) {
    throw new TypeError('Direct ZIP recovery gate requires retained Direct ZIP authority')
  }
  if (state.kind === 'resumable-receive' && state.payloadKind !== 'direct-zip') {
    throw new TypeError('Direct ZIP recovery cannot consume a file-set checkpoint')
  }
}
