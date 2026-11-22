import type { RetainedReceiveAction, RetainedReceiveOperation } from '../contracts'

export type RetainedReceiveOperationSnapshot = Readonly<RetainedReceiveOperation>

export interface PendingRetainedReceiveActionSnapshot {
  readonly operationId: string
  readonly action: RetainedReceiveAction
}

export type RetainedReceiveInventorySnapshot = Readonly<{
  operations: readonly RetainedReceiveOperationSnapshot[]
}> & (
  | Readonly<{ kind: 'unloaded' | 'loading' | 'failed'; pending: null }>
  | Readonly<{ kind: 'empty' | 'available'; pending: PendingRetainedReceiveActionSnapshot | null }>
)

export const EMPTY_RETAINED_INVENTORY: RetainedReceiveInventorySnapshot = Object.freeze({
  kind: 'unloaded',
  operations: Object.freeze([]),
  pending: null,
})
