import type { RetainedReceiveOperationSnapshot } from './model'
import type { RetainedCompatibleNameRepairSource } from '../observation/contracts'
import {
  compatibleNameRepairSummary,
} from '../../../output/file-system-access/compatible-name/model'
import {
  type CompatibleNameRepairSummary,
} from '../../../transfer/lifecycle/name-repair'
import { hasValidatedTerminalCompatibleNameRepair } from '../repair'
import type {
  RetainedReceiveInventory,
  RetainedReceiveAction,
  RetainedReceiveOperation,
} from '../contracts'

export function retainedReceiveActions(
  operation: RetainedReceiveOperation,
  summary: CompatibleNameRepairSummary | undefined,
): readonly RetainedReceiveAction[] {
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
  left: readonly RetainedReceiveAction[],
  right: readonly RetainedReceiveAction[],
): boolean {
  return left.length === right.length && left.every((action, index) => action === right[index])
}

export function retainedReceiveContinuation(
  operation: RetainedReceiveOperation,
  summary: CompatibleNameRepairSummary | undefined,
): RetainedReceiveOperation['continuation'] {
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

export interface ResolvedRetainedInventory {
  readonly source: RetainedReceiveInventory
  readonly operations: readonly RetainedReceiveOperationSnapshot[]
  readonly sourceOperations: ReadonlyMap<
    RetainedReceiveOperationSnapshot,
    RetainedReceiveOperation
  >
}


export async function resolveRetainedInventory(
  loaded: RetainedReceiveInventory,
  signal: AbortSignal,
  repairSource: RetainedCompatibleNameRepairSource | undefined,
): Promise<ResolvedRetainedInventory> {
  const summaries = repairSource === undefined
    ? loaded.operations.map(() => undefined)
    : await Promise.all(loaded.operations.map(operation =>
        operation.continuation === 'cleanup-incompatible'
          ? undefined
          : Promise.resolve(repairSource.readRepairSummary(operation.operationId, signal))))
  signal.throwIfAborted()

  const sourceOperations = new Map<
    RetainedReceiveOperationSnapshot,
    RetainedReceiveOperation
  >()
  const operations: RetainedReceiveOperationSnapshot[] = []
  loaded.operations.forEach((operation, index) => {
    const summary = summaries[index]
    const durableSummary = summary === undefined
      ? undefined
      : compatibleNameRepairSummary(summary)
    const actions = retainedReceiveActions(
      operation,
      durableSummary,
    )
    const continuation = retainedReceiveContinuation(operation, durableSummary)
    let presented: RetainedReceiveOperationSnapshot
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
