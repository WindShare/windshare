import type { ReceiveOperationDisplay } from '../../output/workspace/operation-display'
import type { FailureFact } from '../../diagnostics/incident'
import type {
  OutputFailureBindingLease,
  OutputFailureSinks,
} from '../../output/diagnostics'
import type {
  BoundReceiveIntent,
  CandidateMaterializationBinding,
  EnvironmentOffers,
  OfferedArtifactChoice,
  ResolvedArtifactAction,
} from '../../output/planning'
import type { ReceiveOperationContinuation } from '../../output/resume/descriptor'
import type { ReceiveLifecycleState } from '../../output/workspace'
import type { CompatibleNameRepairProjectionSource } from '../../output/file-system-access/compatible-name/coordinator'
import type {
  RecoverySummary,
} from '../../transfer/lifecycle/recovery-summary'
import type { ArtifactChoiceID, ReceiveIntent } from '../../transfer/intent'
import type { PlanExecutionAuthority } from '../../transfer/output/session'
import type { LifecycleUserAction, ActiveReceiveControl, WorkspaceUsage } from './actions'

export type PresentationSourceOutcome =
  | 'picker_refused'
  | 'caller_cancelled'
  | 'stale_replacement'
  | 'native_fault'

export class PresentationSourceError extends Error {
  readonly outcome: PresentationSourceOutcome

  constructor(outcome: PresentationSourceOutcome, cause: unknown) {
    super('Browser receive source reported a closed presentation outcome', { cause })
    this.name = 'PresentationSourceError'
    this.outcome = outcome
  }
}

export function presentationSourceOutcome(error: unknown): PresentationSourceOutcome {
  return error instanceof PresentationSourceError ? error.outcome : 'native_fault'
}

export interface LifecycleMutation {
  readonly lifecycle: ReceiveLifecycleState
  readonly workspaceUsage?: WorkspaceUsage | null
  readonly activeControls?: readonly ActiveReceiveControl[]
  readonly resumeTransfer?: boolean
  /** Action outcome is separate from the durable state that survived the attempt. */
  readonly actionOutcome?: LifecycleActionOutcome
  readonly recoverySummary?: RecoverySummary
}

export type LifecycleActionOutcome =
  | Readonly<{ kind: 'completed' }>
  | Readonly<{ kind: 'failed'; error: unknown }>

export interface BoundReceiveOperation {
  readonly display?: ReceiveOperationDisplay
  readonly intent: ReceiveIntent
  /** Plans and job identity are replaced together only at an explicit continuation boundary. */
  readonly plans: PlanExecutionAuthority
  readonly transferJobId: string
  readonly lifecycle: ReceiveLifecycleState
  /** Subscribe before reading lifecycle; notifications never settle the execution lifetime. */
  subscribeLifecycle(listener: (state: ReceiveLifecycleState) => void): () => void
  readonly activeControls: readonly ActiveReceiveControl[]
  readonly outputProgress?: DirectZipProgressSource | BrowserFolderProgressSource
  readonly observeCheckpoint?: NonNullable<import('../../transfer/job/contract').TransferJobOptions['onCheckpointObservation']>
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
  interrupt(control: ActiveReceiveControl, transfer: AbortController): void

  /** Picker-bearing publication retries must start inside this synchronous call. */
  startLifecycleAction(
    action: Exclude<LifecycleUserAction, ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): LifecycleMutation | PromiseLike<LifecycleMutation>

  resolveWorkspaceUsage(
    lifecycle: ReceiveLifecycleState,
  ): WorkspaceUsage | null | PromiseLike<WorkspaceUsage | null>
  settleTransferAdmissionFailure(
    reason: unknown,
  ): LifecycleMutation | PromiseLike<LifecycleMutation>
  detach(): void | PromiseLike<void>
}

export interface OwnedActivationAuthority {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState

  /** Durable effects remain owned until settlement records their terminal disposition. */
  settleActivationFailure(reason: unknown): Promise<LifecycleMutation>
  detach(): void | PromiseLike<void>
}

/**
 * A successful bound-operation return is the activation linearization point.
 * Routes must represent uncertain or retained pre-return effects as owned-effects.
 * Retryable-precut certifies both proven absence and that this same presentation
 * authority is ready for another commit; an ordinary rejection ends the authority.
 * Once the durable cut may have occurred, neither retryable-precut nor rejection is valid.
 */
export type RouteCommitResult =
  | Readonly<{
      kind: 'bound-operation'
      operation: BoundReceiveOperation
      display?: ReceiveOperationDisplay
    }>
  | Readonly<{
      kind: 'owned-effects'
      cause: unknown
      authority: OwnedActivationAuthority
    }>
  | Readonly<{
      kind: 'retryable-precut'
      /** Present when candidate preparation minted the receiver-local operation identity. */
      receiverOperationId?: string
    }>

export interface RouteCommitInput {
  readonly display?: ReceiveOperationDisplay
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

export interface ArtifactPresentationAuthority {
  /** Workspace and portable are ready immediately; picker-backed routes settle later. */
  readonly ready: Promise<void>
  /** A retryable-precut result is the only outcome that permits another call. */
  commit(input: RouteCommitInput): Promise<RouteCommitResult>
  /** Non-cancellable picker work remains observed and drained after this pre-commit release. */
  release(reason: unknown): void | PromiseLike<void>
}

export type RetainedReceiveAction =
  | 'save-staged-files'
  | 'cleanup-staging'
  | 'discard-incomplete-staging'
  | 'save-partial'
  | 'continue'
  | 'catch-up'
  | 'save'
  | 'redownload'
  | 'discard'
  | 'delete'
  | 'forget'

export type RetainedReceiveActionResult =
  | Readonly<{ kind: 'completed' }>
  | Readonly<{
      kind: 'receive-continuation'
      runtime: BoundReceiveOperation
    }>

export interface RetainedReceiveOperation {
  readonly display?: ReceiveOperationDisplay
  readonly shareInstance?: string
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly lifecycleGeneration: bigint
  readonly lifecycle: ReceiveLifecycleState
  readonly continuation: ReceiveOperationContinuation
  readonly actions: readonly RetainedReceiveAction[]
  readonly sourceRevisionFailures?: import('../../transfer/lifecycle/source-revision').SourceRevisionFailures
  readonly browserDelivery?: import('../../output/browser-delivery/retained').BrowserDeliveryResumeSummary
  readonly recoverySummary?: RecoverySummary
  readonly unavailableReason?: string
}

export interface RetainedReceiveInventory {
  readonly operations: readonly RetainedReceiveOperation[]
  /** Source-reviewed facts; the coordinator never infers incident admission from React state. */
  readonly presentationFailures: readonly FailureFact[]

  /**
   * The operation object is an opaque inventory-owned token. Implementations reject
   * copied projections so presentation can never reconstruct persisted authority.
   */
  act(
    operation: RetainedReceiveOperation,
    action: RetainedReceiveAction,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): PromiseLike<RetainedReceiveActionResult>
  close(): void
}

export interface RetainedReceiveInventoryPort {
  list(
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): PromiseLike<RetainedReceiveInventory>
  readRepairSummary?(
    operationId: string,
    signal: AbortSignal,
  ): PromiseLike<import('../../transfer/lifecycle/name-repair').CompatibleNameRepairSummary | undefined>
}

export interface ReceiveCompositionPort {
  readonly retained: RetainedReceiveInventoryPort

  environment(signal: AbortSignal): EnvironmentOffers | PromiseLike<EnvironmentOffers>

  /** The implementation must start any picker before this call returns. */
  startArtifactAuthority(
    offered: OfferedArtifactChoice,
    preClickRanking: readonly ArtifactChoiceID[],
    failures?: OutputFailureSinks,
    display?: ReceiveOperationDisplay,
    recoveryPreference?: import('../../output/browser-delivery/model').BrowserRecoveryPreference,
  ): ArtifactPresentationAuthority
}

export interface BrowserFolderProgressSnapshot {
  readonly kind: 'browser-folder'
  readonly operationId: string
  readonly generation: bigint
  readonly summary: import('../../output/browser-delivery/retained').BrowserDeliveryResumeSummary
}

export interface BrowserFolderProgressSource {
  getSnapshot(): BrowserFolderProgressSnapshot
  subscribe(listener: (snapshot: BrowserFolderProgressSnapshot) => void): () => void
}

export type DirectZipProgressPhase =
  | 'receiving'
  | 'saving-resume-position'
  | 'closing'
  | 'verifying'

export interface DirectZipProgressSnapshot {
  readonly kind: 'direct-zip'
  readonly operationId: string
  readonly generation: bigint
  readonly phase: DirectZipProgressPhase
  /** Logical payload, including the retained prefix; retries do not count twice. */
  readonly receivedSelectedBytes: bigint
  /** Payload acknowledged by the output writer; acknowledgement alone does not prove recovery. */
  readonly writtenSelectedBytes: bigint
  readonly safeResumeBytes: bigint
  readonly resumeTemporarySpaceUpperBound?: bigint
}

/** The operation owns this stream; UI observers never derive safe bytes from transfer acknowledgements. */
export interface DirectZipProgressSource {
  getSnapshot(): DirectZipProgressSnapshot
  subscribe(listener: (snapshot: DirectZipProgressSnapshot) => void): () => void
}
