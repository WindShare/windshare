import type { FailureFact } from '../diagnostics/incident'
import type {
  OutputFailureBindingLease,
  OutputFailureSinks,
} from '../output/diagnostics'
import type {
  BoundReceiveIntent,
  CandidateMaterializationBinding,
  EnvironmentOffers,
  OfferedArtifactChoice,
  ResolvedArtifactAction,
} from '../output/planning'
import type { ReceiveOperationContinuation } from '../output/resume/descriptor'
import type { ReceiveLifecycleState } from '../output/workspace'
import type { CompatibleNameRepairProjectionSource } from '../output/file-system-access/compatible-name/coordinator'
import type { ReceiveIntent } from '../transfer/intent'
import type { V2PlanExecutionAuthority } from '../transfer/output-session'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from './v2-lifecycle-presentation'

export type V2PresentationSourceOutcome =
  | 'picker_refused'
  | 'caller_cancelled'
  | 'stale_replacement'
  | 'native_fault'

export class V2PresentationSourceError extends Error {
  readonly outcome: V2PresentationSourceOutcome

  constructor(outcome: V2PresentationSourceOutcome, cause: unknown) {
    super('Browser receive source reported a closed presentation outcome', { cause })
    this.name = 'V2PresentationSourceError'
    this.outcome = outcome
  }
}

export function presentationSourceOutcome(error: unknown): V2PresentationSourceOutcome {
  return error instanceof V2PresentationSourceError ? error.outcome : 'native_fault'
}

export interface V2LifecycleMutation {
  readonly lifecycle: ReceiveLifecycleState
  readonly workspaceUsage?: WorkspaceUsage | null
  readonly activeControls?: readonly V2ActiveReceiveControl[]
  readonly resumeTransfer?: boolean
}

export interface V2BoundReceiveOperation {
  readonly intent: ReceiveIntent
  /** Plans and job identity are replaced together only at an explicit continuation boundary. */
  readonly plans: V2PlanExecutionAuthority
  readonly transferJobId: string
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls: readonly V2ActiveReceiveControl[]
  readonly initialWorkspaceUsage?: WorkspaceUsage | null
  readonly repairProjection?: CompatibleNameRepairProjectionSource
  subscribeRepairProjectionActivation?(
    listener: (source: CompatibleNameRepairProjectionSource) => void,
  ): () => void

  /**
   * The runtime-local binding prevents output sessions that span retries from
   * retaining the authority of whichever presentation attempt created them.
   */
  bindOutputFailures?(failures: OutputFailureSinks | undefined): OutputFailureBindingLease

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

export interface V2OwnedActivationAuthority {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState

  /** Durable effects remain owned until settlement records their terminal disposition. */
  settleActivationFailure(reason: unknown): Promise<V2LifecycleMutation>
  detach(): void | PromiseLike<void>
}

/**
 * A successful bound-operation return is the activation linearization point.
 * Routes must represent uncertain or retained pre-return effects as owned-effects.
 * Retryable-precut certifies both proven absence and that this same presentation
 * authority is ready for another commit; an ordinary rejection ends the authority.
 * Once the durable cut may have occurred, neither retryable-precut nor rejection is valid.
 */
export type V2RouteCommitResult =
  | Readonly<{
      kind: 'bound-operation'
      operation: V2BoundReceiveOperation
    }>
  | Readonly<{
      kind: 'owned-effects'
      cause: unknown
      authority: V2OwnedActivationAuthority
    }>
  | Readonly<{
      kind: 'retryable-precut'
      /** Present when candidate preparation minted the receiver-local operation identity. */
      receiverOperationId?: string
    }>

export interface V2RouteCommitInput {
  readonly action: ResolvedArtifactAction
  /** Attempt cancellation is separate from releasing the reusable presentation authority. */
  readonly signal: AbortSignal

  /**
   * The route supplies only canonical candidate data. The coordinator owns the
   * final identity/observation fence and is the sole receive-intent binder.
   */
  readonly freezeAtFence: (
    candidate: CandidateMaterializationBinding,
  ) => Promise<BoundReceiveIntent>
}

export interface V2ArtifactPresentationAuthority {
  /** Workspace and portable are ready immediately; picker-backed routes settle later. */
  readonly ready: Promise<void>
  /** A retryable-precut result is the only outcome that permits another call. */
  commit(input: V2RouteCommitInput): Promise<V2RouteCommitResult>
  /** Non-cancellable picker work remains observed and drained after this pre-commit release. */
  release(reason: unknown): void | PromiseLike<void>
}

export type V2RetainedReceiveAction =
  | 'continue'
  | 'catch-up'
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
  /** Source-reviewed facts; the coordinator never infers incident admission from React state. */
  readonly presentationFailures: readonly FailureFact[]

  /**
   * The operation object is an opaque inventory-owned token. Implementations reject
   * copied projections so presentation can never reconstruct persisted authority.
   */
  act(
    operation: V2RetainedReceiveOperation,
    action: V2RetainedReceiveAction,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): PromiseLike<V2RetainedReceiveActionResult>
  close(): void
}

export interface V2RetainedReceiveInventoryPort {
  list(
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): PromiseLike<V2RetainedReceiveInventory>
  readRepairSummary?(
    operationId: string,
    signal: AbortSignal,
  ): PromiseLike<import('../output/file-system-access/compatible-name/model').CompatibleNameRepairSummary | undefined>
}

export interface V2ReceiveCompositionPort {
  readonly retained: V2RetainedReceiveInventoryPort

  environment(signal: AbortSignal): EnvironmentOffers | PromiseLike<EnvironmentOffers>

  /** The implementation must start any picker before this call returns. */
  startArtifactAuthority(
    offered: OfferedArtifactChoice,
    failures?: OutputFailureSinks,
  ): V2ArtifactPresentationAuthority
}
