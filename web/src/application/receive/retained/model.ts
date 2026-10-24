import type {
  CompatibleNameRepairSummary,
} from '../../../transfer/lifecycle/name-repair'
import type { V2RetainedReceiveAction, V2RetainedReceiveOperation } from '../contracts'

export type RetainedReceiveOperationSnapshot = Readonly<
  V2RetainedReceiveOperation & { readonly repairSummary?: CompatibleNameRepairSummary }
>

export interface V2PendingRetainedReceiveActionSnapshot {
  readonly operationId: string
  readonly action: V2RetainedReceiveAction
}

export type V2RetainedReceiveInventorySnapshot =
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
      pending: V2PendingRetainedReceiveActionSnapshot | null
    }>
  | Readonly<{
      kind: 'failed'
      operations: readonly RetainedReceiveOperationSnapshot[]
      error: string
      pending: null
    }>

export const EMPTY_V2_RETAINED_INVENTORY: V2RetainedReceiveInventorySnapshot = Object.freeze({
  kind: 'loading',
  operations: Object.freeze([]),
  error: null,
  pending: null,
})
