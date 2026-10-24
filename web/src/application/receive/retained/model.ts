import type {
  CompatibleNameRepairSummary,
} from '../../../transfer/lifecycle/name-repair'
import type { RetainedReceiveAction, RetainedReceiveOperation } from '../contracts'

export type RetainedReceiveOperationSnapshot = Readonly<
  RetainedReceiveOperation & { readonly repairSummary?: CompatibleNameRepairSummary }
>

export interface PendingRetainedReceiveActionSnapshot {
  readonly operationId: string
  readonly action: RetainedReceiveAction
}

export type RetainedReceiveInventorySnapshot =
  | Readonly<{
      kind: 'loading'
      operations: readonly RetainedReceiveOperationSnapshot[]
      error: null
      pending: null
    }>
  | Readonly<{
      kind: 'ready'
      operations: readonly RetainedReceiveOperationSnapshot[]
      error: null
      pending: PendingRetainedReceiveActionSnapshot | null
    }>
  | Readonly<{
      kind: 'failed'
      operations: readonly RetainedReceiveOperationSnapshot[]
      error: string
      pending: null
    }>

export const EMPTY_RETAINED_INVENTORY: RetainedReceiveInventorySnapshot = Object.freeze({
  kind: 'loading',
  operations: Object.freeze([]),
  error: null,
  pending: null,
})
