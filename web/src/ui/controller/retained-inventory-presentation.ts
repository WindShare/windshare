import type { CompatibleNameRepairSummary } from '../../output/file-system-access/compatible-name/model'
import type {
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'

export function retainedPresentationActions(
  operation: V2RetainedReceiveOperation,
  pendingCatchUp: boolean,
): readonly V2RetainedReceiveAction[] {
  if (pendingCatchUp) {
    return operation.actions.includes('catch-up')
      ? Object.freeze(['catch-up'])
      : Object.freeze([])
  }
  if (operation.continuation === 'pending-catch-up' ||
      operation.continuation === 'restoration-available') {
    return Object.freeze([])
  }
  return Object.freeze(operation.actions.filter(action => action !== 'catch-up'))
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
  const repairedTreeState = operation.lifecycle.kind === 'receiving' ||
    operation.lifecycle.kind === 'published' || operation.lifecycle.kind === 'partial-directory'
  if (repairedTreeState && summary?.pendingCatchUp === true) return 'pending-catch-up'
  const footer = summary?.latestObservedFooter
  if ((operation.lifecycle.kind === 'published' || operation.lifecycle.kind === 'partial-directory') &&
      summary?.pendingCatchUp === false && footer !== undefined && footer.state !== 'active' &&
      footer.committedCount === summary.committedCount) {
    return 'restoration-available'
  }
  return operation.continuation
}
