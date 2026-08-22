import {
  nextReceiveLifecycleState,
  type NeedsAttentionReason,
  type PlanKind,
  type ReceiveLifecycleState,
} from '../state'
import type { LifecycleReduction } from './events'

export function activeLeaseMismatch(state: ReceiveLifecycleState, leaseId: string): boolean {
  return 'activeLeaseId' in state && state.activeLeaseId !== leaseId
}

export function requireWorkspaceState<K extends ReceiveLifecycleState['kind']>(
  state: ReceiveLifecycleState,
  planKind: PlanKind,
  kind: K,
): asserts state is Extract<ReceiveLifecycleState, { kind: K }> {
  if (planKind !== 'workspace-then-publish') {
    throw new TypeError('event is exclusive to WorkspaceThenPublish')
  }
  requireState(state, kind)
}

export function requireState<K extends ReceiveLifecycleState['kind']>(
  state: ReceiveLifecycleState,
  kind: K,
): asserts state is Extract<ReceiveLifecycleState, { kind: K }> {
  if (state.kind !== kind) throw new TypeError(`event is not legal from ${state.kind}`)
}

export function requireClock(nowMilliseconds: number): void {
  if (!Number.isSafeInteger(nowMilliseconds) || nowMilliseconds < 0) {
    throw new TypeError('lifecycle clock must be a non-negative safe integer')
  }
}

export function applied(state: ReceiveLifecycleState): LifecycleReduction {
  return Object.freeze({ status: 'applied', state })
}

export function needsAttention(
  state: ReceiveLifecycleState,
  reason: NeedsAttentionReason,
  lastVerifiedRecordDigest: string,
): ReceiveLifecycleState {
  return nextReceiveLifecycleState(state, {
    kind: 'needs-attention',
    reason,
    lastVerifiedRecordDigest,
  })
}
