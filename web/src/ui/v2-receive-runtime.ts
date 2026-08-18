import type {
  AcquiredMaterializationAuthority,
  ArtifactAction,
  EnvironmentOffers,
} from '../output/planning'
import type { ReceiveOperationContinuation } from '../output/resume/descriptor'
import type { ReceiveLifecycleState } from '../output/workspace'
import type { ReceiveIntent } from '../transfer/intent'
import type { V2PlanExecutionAuthority } from '../transfer/output-session'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from './v2-lifecycle-presentation'

export interface V2LifecycleMutation {
  readonly lifecycle: ReceiveLifecycleState
  readonly workspaceUsage?: WorkspaceUsage | null
  readonly activeControls?: readonly V2ActiveReceiveControl[]
  readonly resumeTransfer?: boolean
}

/**
 * The authority adapter owns handles and persistence, while the callback keeps the
 * epoch/capability recheck in the controller immediately before intent persistence.
 */
export interface V2StartedArtifactAuthority {
  finalize(
    freezeIntent: (
      acquired: AcquiredMaterializationAuthority,
    ) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation>
  release(reason: unknown): void | PromiseLike<void>
}

export interface V2BoundReceiveOperation {
  readonly intent: ReceiveIntent
  /** Plans and job identity are replaced together only at an explicit continuation boundary. */
  readonly plans: V2PlanExecutionAuthority
  readonly transferJobId: string
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls: readonly V2ActiveReceiveControl[]
  readonly initialWorkspaceUsage?: WorkspaceUsage | null

  /** The implementation translates the product control into its plan-specific stable cut. */
  interrupt(control: V2ActiveReceiveControl, transfer: AbortController): void

  /** Picker-bearing publication retries must start inside this synchronous call. */
  startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): V2LifecycleMutation | PromiseLike<V2LifecycleMutation>

  observeExpiry(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation>
  resolveWorkspaceUsage(
    lifecycle: ReceiveLifecycleState,
  ): WorkspaceUsage | null | PromiseLike<WorkspaceUsage | null>
  settleTransferAdmissionFailure(
    reason: unknown,
  ): V2LifecycleMutation | PromiseLike<V2LifecycleMutation>
  detach(): void | PromiseLike<void>
}

export type V2RetainedReceiveAction =
  | 'continue'
  | 'save'
  | 'redownload'
  | 'discard'
  | 'delete'

export type V2RetainedReceiveActionResult =
  | Readonly<{ kind: 'completed' }>
  | Readonly<{
      kind: 'receive-continuation'
      runtime: V2BoundReceiveOperation
    }>

export interface V2RetainedReceiveOperation {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly lifecycleGeneration: bigint
  readonly lifecycle: ReceiveLifecycleState
  readonly continuation: ReceiveOperationContinuation
  readonly expiresAt?: number
  readonly actions: readonly V2RetainedReceiveAction[]
  readonly unavailableReason?: string
}

export interface V2RetainedReceiveInventory {
  readonly operations: readonly V2RetainedReceiveOperation[]

  /**
   * The operation object is an opaque inventory-owned token. Implementations reject
   * copied projections so presentation can never reconstruct persisted authority.
   */
  act(
    operation: V2RetainedReceiveOperation,
    action: V2RetainedReceiveAction,
    signal: AbortSignal,
  ): PromiseLike<V2RetainedReceiveActionResult>
  close(): void
}

export interface V2RetainedReceiveInventoryPort {
  list(signal: AbortSignal): PromiseLike<V2RetainedReceiveInventory>
}

export interface V2ReceiveCompositionPort {
  readonly retained: V2RetainedReceiveInventoryPort

  environment(signal: AbortSignal): EnvironmentOffers | PromiseLike<EnvironmentOffers>

  /** The implementation must start any picker before this call returns. */
  startArtifactAuthority(
    action: ArtifactAction,
  ): V2StartedArtifactAuthority | PromiseLike<V2StartedArtifactAuthority>
}
