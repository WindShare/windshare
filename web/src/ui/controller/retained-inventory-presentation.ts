import type { CompatibleNameRepairSummary } from '../../output/file-system-access/compatible-name/model'
import {
  hasValidatedTerminalCompatibleNameRepair,
} from '../compatible-name-repair-presentation'
import type {
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'

export function retainedPresentationActions(
  operation: V2RetainedReceiveOperation,
  summary: CompatibleNameRepairSummary | undefined,
): readonly V2RetainedReceiveAction[] {
  if (summary?.terminalSettlement === 'pending') {
    return Object.freeze(operation.actions.filter(action => action === 'catch-up'))
  }
  const needsCatchUp = summary?.sidecarSync === 'pending'
  // Sidecar replay and download continuation have separate authority. A stale
  // projection must never remove a valid receive continuation.
  return Object.freeze(operation.actions.filter(action =>
    action !== 'catch-up' || needsCatchUp))
}

export function sameRetainedActions(
  left: readonly V2RetainedReceiveAction[],
  right: readonly V2RetainedReceiveAction[],
): boolean {
  return left.length === right.length && left.every((action, index) => action === right[index])
}

export function retainedPresentationContinuation(
  operation: V2RetainedReceiveOperation,
  summary: CompatibleNameRepairSummary | undefined,
): V2RetainedReceiveOperation['continuation'] {
  if (summary?.terminalSettlement === 'pending') return 'pending-catch-up'
  if (operation.lifecycle.kind !== 'published' && operation.lifecycle.kind !== 'partial-directory') {
    return operation.continuation
  }
  if (summary?.sidecarSync === 'pending') {
    return 'pending-catch-up'
  }
  return summary !== undefined && hasValidatedTerminalCompatibleNameRepair(summary)
    ? 'restoration-available'
    : operation.continuation
}
