import type { AuthenticatedGenerationReference } from '../output/workspace/manifest'
import type { PreparationManifestEntry } from '../output/workspace/preparation'
import type { ReceiveLifecycleState } from '../output/workspace/state'
import type { CompatibleNameRepairSummary } from '../output/file-system-access/compatible-name/model'
import type { PerformanceSummaryObservations } from '../output/diagnostics/performance-summary'
import {
  FaultScope,
  isFault,
  type Fault,
} from './fault'
import {
  snapshotMaterializationDirectory,
  type CanonicalModifiedTime,
  type DirectoryAdmission,
  type DirectorySettlement,
  type MaterializationDirectory,
} from './directory-admission'
import type {
  DirectAtomicPlan,
  DirectResumableZipPlan,
  DirectTreePlan,
  MaterializationPlan,
  OriginalFileArtifact,
  PortableHandoffPlan,
  ReceiveIntent,
  WorkspaceThenPublishPlan,
  ZipArchiveArtifact,
} from './intent'
import type {
  DirectZipIntent,
  DirectZipOrderedOutputV1,
  DirectZipOutputSessionV1,
} from './direct-zip/model'
import type {
  CompletedTransferWorkerSettlement,
  PausedTransferWorkerSettlement,
  SuccessfulTransferWorkerSettlement,
  TransferWorkerSettlement,
} from './outcome'
import type { AuthenticatedLogicalSiblingMembership } from './job/contract'
import {
  snapshotLogicalArtifactPath,
  snapshotSourceAuthenticationPath,
  type LogicalArtifactPath,
  type SourceAuthenticationPath,
} from './job/coordinate/direct-tree'
import {
  OutputSessionBindingError,
  outputCapabilities,
  outputExecutionProfile,
  outputSessionIdentity,
  type OutputSession,
} from './output-file-contract'

export * from './output-file-contract'

export interface DirectoryMaterializationRequest {
  readonly directory: MaterializationDirectory
  readonly sourceAuthenticationPath: SourceAuthenticationPath
  readonly logicalArtifactPath: LogicalArtifactPath
  /** Lazy authority; carrying it must not itself query catalog page storage. */
  readonly logicalSiblingMembership?: AuthenticatedLogicalSiblingMembership
}

/** Incremental directory authority is exclusive to native/tree materialization. */
export interface IncrementalDirectoryOutput {
  admitDirectory(
    directory: DirectoryMaterializationRequest,
    signal: AbortSignal,
  ): Promise<DirectoryAdmission>
  finalizeDirectory(admission: DirectoryAdmission, signal: AbortSignal): Promise<DirectorySettlement>
}

export interface MaterializationSummary {
  readonly entryCount: bigint
  readonly fileCount: bigint
  readonly directoryCount: bigint
  readonly rawBytes: bigint
}

export interface PlanSettlementRequest<
  Settlement extends TransferWorkerSettlement = TransferWorkerSettlement,
> {
  readonly transferJobId: string
  readonly worker: Settlement
  readonly materialization: MaterializationSummary
}

export interface PlanPauseRequest {
  readonly worker: TransferWorkerSettlement
  readonly materialization: MaterializationSummary
  readonly reason: unknown
}

/** Explicit terminal ownership cut for DirectTree output that remains user-owned. */
export interface PlanStopRequest {
  readonly transferJobId: string
  readonly worker: PausedTransferWorkerSettlement
  readonly materialization: MaterializationSummary
  readonly reason: TransferStopRequestedError
}

interface PlanExecutionBase<
  Plan extends MaterializationPlan,
  Output extends Pick<OutputSession, 'identity' | 'capabilities'> = OutputSession,
> {
  readonly planKind: Plan['kind']
  readonly output: Output
  pause(request: PlanPauseRequest, signal: AbortSignal): Promise<ReceiveLifecycleState>
}

export interface DirectTreeExecution extends PlanExecutionBase<DirectTreePlan> {
  readonly planKind: 'direct-tree'
  readonly directories: IncrementalDirectoryOutput
  readonly performance?: PerformanceSummaryObservations
  readonly repairSummary?: () => CompatibleNameRepairSummary | undefined
  readonly terminalSettlementInitiated?: () => boolean
  beginTerminal(kind: 'pause' | 'stop' | 'settle'): void
  settle(
    request: PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  stop?(request: PlanStopRequest, signal: AbortSignal): Promise<ReceiveLifecycleState>
}

export interface DirectAtomicExecution extends PlanExecutionBase<DirectAtomicPlan> {
  readonly planKind: 'direct-atomic'
  settle(
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}

export interface DirectResumableZipExecution extends
  PlanExecutionBase<DirectResumableZipPlan, DirectZipOutputSessionV1> {
  readonly planKind: 'direct-resumable-zip'
  readonly ordered: DirectZipOrderedOutputV1
  settle(
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}

export interface WorkspaceExecution extends PlanExecutionBase<WorkspaceThenPublishPlan> {
  readonly planKind: 'workspace-then-publish'
  settle(
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}

export interface PortableExecution extends PlanExecutionBase<PortableHandoffPlan> {
  readonly planKind: 'portable-handoff'
  settle(
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}

export type PlanExecution =
  | DirectTreeExecution
  | DirectAtomicExecution
  | DirectResumableZipExecution
  | WorkspaceExecution
  | PortableExecution

export interface ExactPreparationEvidence {
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly PreparationManifestEntry[]
  readonly entryCount: bigint
  readonly fileCount: bigint
  readonly directoryCount: bigint
  readonly selectedRawBytes: bigint
}

/**
 * Authenticated catalog authority needed for Workspace OriginalFile admission.
 * Resolving this one path is not a whole-selection preparation barrier and must
 * occur before the adapter can allocate output or request the file revision.
 */
export interface ExactSingleFileEvidence {
  readonly fileId: string
  readonly containingDirectoryId: string
  readonly generation: string
  readonly catalogSize: bigint
  readonly sourcePath: readonly string[]
  readonly modifiedTime?: CanonicalModifiedTime
}

export type ExecutionAdmissionResult<Execution extends WorkspaceExecution | PortableExecution> =
  | Readonly<{ kind: 'accepted'; execution: Execution }>
  | Readonly<{ kind: 'rejected'; state: ReceiveLifecycleState }>

export type PreparationExecutionResult<Execution extends WorkspaceExecution | PortableExecution> =
  ExecutionAdmissionResult<Execution>

type ReceiveIntentForPlan<Plan extends MaterializationPlan> = ReceiveIntent & Readonly<{ plan: Plan }>
type ReceiveIntentForPlanArtifact<
  Plan extends MaterializationPlan,
  Artifact extends ReceiveIntent['artifact'],
> = ReceiveIntentForPlan<Plan> & Readonly<{ artifact: Artifact }>

/**
 * Controller composition supplies one explicit adapter per immutable plan. The
 * transfer runtime cannot choose a backend, invoke a picker, or infer a format.
 */
export interface V2PlanExecutionAuthority {
  openDirectTree(
    intent: ReceiveIntentForPlan<DirectTreePlan>,
    signal: AbortSignal,
  ): Promise<DirectTreeExecution>
  openDirectAtomic(
    intent: ReceiveIntentForPlan<DirectAtomicPlan>,
    signal: AbortSignal,
  ): Promise<DirectAtomicExecution>
  openDirectResumableZip(
    intent: DirectZipIntent,
    signal: AbortSignal,
  ): Promise<DirectResumableZipExecution>
  openWorkspaceOriginal(
    intent: ReceiveIntentForPlanArtifact<WorkspaceThenPublishPlan, OriginalFileArtifact>,
    evidence: ExactSingleFileEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>>
  prepareWorkspaceZip(
    intent: ReceiveIntentForPlanArtifact<WorkspaceThenPublishPlan, ZipArchiveArtifact>,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<PreparationExecutionResult<WorkspaceExecution>>
  preparePortable(
    intent: ReceiveIntentForPlan<PortableHandoffPlan>,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<PreparationExecutionResult<PortableExecution>>
  settleExecutionAdmissionFailure(
    intent: ReceiveIntent,
    reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  recordSettlementUnknown(
    intent: ReceiveIntent,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>>
}

/** Backend-authored classification for a directory mutation that may be isolated safely. */
export class OutputDirectoryMutationError extends Error {
  readonly sessionCompromised: boolean

  constructor(message: string, sessionCompromised: boolean, options?: ErrorOptions) {
    super(message, options)
    this.name = 'OutputDirectoryMutationError'
    this.sessionCompromised = sessionCompromised
  }
}

/** The backend cannot prove cleanup after acquiring mutable output authority. */
export class OutputSessionCompromisedError extends Error {
  constructor(message: string, options: ErrorOptions) {
    super(message, options)
    this.name = 'OutputSessionCompromisedError'
  }
}

export function validatePlanExecutionBinding<Execution extends PlanExecution>(
  intent: ReceiveIntent,
  execution: Execution,
): Execution {
  if (execution.planKind !== intent.plan.kind) {
    throw new OutputSessionBindingError('plan execution does not match the frozen receive intent')
  }
  outputSessionIdentity(execution.output.identity)
  outputCapabilities(execution.output.capabilities)
  if (execution.planKind !== 'direct-resumable-zip') {
    outputExecutionProfile(execution.output.executionProfile)
  }
  if (intent.plan.kind === 'direct-tree' && !hasDirectoryPort(execution)) {
    throw new OutputSessionBindingError('DirectTree execution requires incremental directory authority')
  }
  if (intent.plan.kind === 'direct-resumable-zip' && !hasOrderedDirectZipPort(execution)) {
    throw new OutputSessionBindingError('DirectResumableZip execution requires ordered archive authority')
  }
  return execution
}

function hasOrderedDirectZipPort(execution: PlanExecution): boolean {
  if (!('ordered' in execution) || execution.ordered === undefined) return false
  return typeof execution.ordered.beginTraversal === 'function' &&
    typeof execution.ordered.visit === 'function' &&
    typeof execution.ordered.finishTraversal === 'function' &&
    typeof execution.ordered.materializationSummary === 'function'
}

function hasDirectoryPort(execution: PlanExecution): boolean {
  if (!('directories' in execution) || execution.directories === undefined) return false
  return typeof execution.directories.admitDirectory === 'function' &&
    typeof execution.directories.finalizeDirectory === 'function'
}

export class TransferPauseRequestedError extends Error {
  constructor(message = 'Transfer paused by receiver', options?: ErrorOptions) {
    super(message, options)
    this.name = 'TransferPauseRequestedError'
  }
}

/** Stop retains the materialized DirectTree prefix and therefore must never enter Pause cleanup. */
export class TransferStopRequestedError extends Error {
  constructor(message = 'Transfer stopped by receiver and partial output retained') {
    super(message)
    this.name = 'TransferStopRequestedError'
  }
}

export function snapshotDirectoryMaterializationRequest(
  request: DirectoryMaterializationRequest,
): DirectoryMaterializationRequest {
  const directory = snapshotMaterializationDirectory(request.directory)
  const logicalSiblingMembership = request.logicalSiblingMembership === undefined
    ? undefined
    : snapshotLogicalSiblingMembership(request.logicalSiblingMembership, directory)
  return Object.freeze({
    directory,
    sourceAuthenticationPath: snapshotSourceAuthenticationPath(request.sourceAuthenticationPath),
    logicalArtifactPath: snapshotLogicalArtifactPath(request.logicalArtifactPath),
    ...(logicalSiblingMembership === undefined ? {} : { logicalSiblingMembership }),
  })
}

function snapshotLogicalSiblingMembership(
  membership: AuthenticatedLogicalSiblingMembership,
  source: MaterializationDirectory,
): AuthenticatedLogicalSiblingMembership {
  if (membership.directoryId !== source.directoryId || membership.generation !== source.generation) {
    throw new OutputSessionBindingError(
      'logical-sibling membership does not match the admitted directory generation',
    )
  }
  if (typeof membership.hasCommittedName !== 'function') {
    throw new OutputSessionBindingError('logical-sibling membership has no committed-name authority')
  }
  return Object.freeze({
    directoryId: source.directoryId,
    generation: source.generation,
    hasCommittedName: (candidate: string) => membership.hasCommittedName(candidate),
  })
}

export function needsAttentionFault(fault: Fault): Fault {
  if (!isFault(fault) ||
      (fault.scope !== FaultScope.OutputPause && fault.scope !== FaultScope.SessionTerminal)) {
    throw new TypeError('needs-attention state requires a job-scoped fault')
  }
  return Object.freeze({ ...fault })
}
