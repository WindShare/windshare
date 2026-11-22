import type { RetainedReceiveOperationSnapshot } from './model'
import type {
  RetainedReceiveInventory,
  RetainedReceiveOperation,
} from '../contracts'

export interface ResolvedRetainedInventory {
  readonly source: RetainedReceiveInventory
  readonly operations: readonly RetainedReceiveOperationSnapshot[]
  readonly sourceOperations: ReadonlyMap<
    RetainedReceiveOperationSnapshot,
    RetainedReceiveOperation
  >
}

export function resolveRetainedInventory(
  loaded: RetainedReceiveInventory,
): ResolvedRetainedInventory {
  const sourceOperations = new Map<
    RetainedReceiveOperationSnapshot,
    RetainedReceiveOperation
  >()
  const operations: RetainedReceiveOperationSnapshot[] = []
  for (const operation of loaded.operations) {
    sourceOperations.set(operation, operation)
    operations.push(operation)
  }
  return Object.freeze({
    source: loaded,
    operations: Object.freeze(operations),
    sourceOperations,
  })
}
