import type { V2RetainedReceivePresentationOperation } from '../v2-model'
import type { V2RetainedCompatibleNameRepairSource } from './contracts'
import { compatibleNameRepairSummary, type CompatibleNameRepairSummary } from '../../output/file-system-access/compatible-name/model'
import {
  hasValidatedTerminalCompatibleNameRepair,
} from '../compatible-name-repair-presentation'
import type {
  V2RetainedReceiveInventory,
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'

export function retainedPresentationActions(
  operation: V2RetainedReceiveOperation,
  summary: CompatibleNameRepairSummary | undefined,
): readonly V2RetainedReceiveAction[] {
  if (summary?.terminalSettlement === 'pending') {
    return Object.freeze(operation.actions.filter(action => action === 'catch-up' || action === 'cleanup-staging' ||
      action === 'discard-incomplete-staging'))
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

export interface PresentedRetainedInventory {
  readonly source: V2RetainedReceiveInventory
  readonly operations: readonly V2RetainedReceivePresentationOperation[]
  readonly sourceOperations: ReadonlyMap<
    V2RetainedReceivePresentationOperation,
    V2RetainedReceiveOperation
  >
}


export async function presentRetainedInventory(
  loaded: V2RetainedReceiveInventory,
  signal: AbortSignal,
  repairSource: V2RetainedCompatibleNameRepairSource | undefined,
): Promise<PresentedRetainedInventory> {
  const summaries = repairSource === undefined
    ? loaded.operations.map(() => undefined)
    : await Promise.all(loaded.operations.map(operation =>
        operation.continuation === 'cleanup-incompatible'
          ? undefined
          : Promise.resolve(repairSource.readRepairSummary(operation.operationId, signal))))
  signal.throwIfAborted()

  const sourceOperations = new Map<
    V2RetainedReceivePresentationOperation,
    V2RetainedReceiveOperation
  >()
  const operations: V2RetainedReceivePresentationOperation[] = []
  loaded.operations.forEach((operation, index) => {
    const summary = summaries[index]
    const durableSummary = summary === undefined
      ? undefined
      : compatibleNameRepairSummary(summary)
    const actions = retainedPresentationActions(
      operation,
      durableSummary,
    )
    const continuation = retainedPresentationContinuation(operation, durableSummary)
    let presented: V2RetainedReceivePresentationOperation
    if (durableSummary === undefined && continuation === operation.continuation &&
        sameRetainedActions(actions, operation.actions)) {
      presented = operation
    } else {
      presented = Object.freeze({
        ...operation,
        continuation,
        actions,
        ...(durableSummary === undefined ? {} : { repairSummary: durableSummary }),
        ...(continuation === 'pending-catch-up' && durableSummary?.sidecarSync === 'current' &&
            durableSummary.terminalSettlement === 'none'
          ? { unavailableReason: 'The prior receive ended abnormally; use the restoration command only after confirming it will not resume.' }
          : {}),
      })
    }
    sourceOperations.set(presented, operation)
    operations.push(presented)
  })
  return Object.freeze({
    source: loaded,
    operations: Object.freeze(operations),
    sourceOperations,
  })
}
