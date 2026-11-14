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
      kind: 'unloaded'
      operations: readonly RetainedReceiveOperationSnapshot[]
      error: null
      pending: null
    }>
  | Readonly<{
      kind: 'loading'
      operations: readonly RetainedReceiveOperationSnapshot[]
      error: null
      pending: null
    }>
  | Readonly<{
      kind: 'empty' | 'available'
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
  kind: 'unloaded',
  operations: Object.freeze([]),
  error: null,
  pending: null,
})
