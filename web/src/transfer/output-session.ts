import { ByteRangeSet, type ByteRange } from '../content/geometry'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import type { AuthenticatedGenerationReference } from '../output/workspace/manifest'
import type { PreparationManifestEntry } from '../output/workspace/preparation'
import type { ReceiveLifecycleState } from '../output/workspace/state'
import {
  FaultScope,
  isFault,
  type Fault,
} from './fault'
import {
  MAX_MATERIALIZATION_PATH_BYTES,
  snapshotCanonicalModifiedTime,
  snapshotDirectoryAdmission,
  snapshotMaterializationDirectory,
  snapshotMaterializationPath,
  type CanonicalModifiedTime,
  type DirectoryAdmission,
  type DirectorySettlement,
  type MaterializationDirectory,
} from './directory-admission'
import type {
  DirectAtomicPlan,
  DirectTreePlan,
  MaterializationPlan,
  OriginalFileArtifact,
  PortableHandoffPlan,
  ReceiveIntent,
  WorkspaceThenPublishPlan,
  ZipArchiveArtifact,
} from './intent'
import type {
  CompletedTransferWorkerSettlement,
  SuccessfulTransferWorkerSettlement,
  TransferWorkerSettlement,
} from './outcome'

export type DurabilityLevel = 'None' | 'ProcessRestart' | 'PowerLoss'
export type FileRetirementDisposition = 'FileIsolated' | 'JobOutputCompromised'

export const MAXIMUM_OPEN_OUTPUT_FILES = 32
export const MAXIMUM_OUTPUT_IDENTITY_BYTES = 128
export const MAXIMUM_OWNED_FILE_IDENTITY_BYTES = MAX_MATERIALIZATION_PATH_BYTES + 256
export const MAXIMUM_VERIFIED_DURABLE_RANGES = 16_384
const OUTPUT_CATALOG_IDENTITY_BYTES = 16

export interface OutputCapabilities {
  readonly durability: DurabilityLevel
  readonly randomWrite: boolean
  readonly fileFailureIsolation: boolean
  readonly modificationTime: boolean
}

export interface OutputCatalogFileIdentity {
  readonly shareInstance: string
  readonly fileId: string
}

export interface OutputSourceIdentity extends OutputCatalogFileIdentity {
  readonly fileRevision: string
}

export interface OpenedOutputRevision extends OutputSourceIdentity {
  readonly exactSize: bigint
}

export interface OutputSessionIdentity {
  readonly backend: string
  readonly outputSessionId: string
}

export interface OutputFileOwnership extends OutputSessionIdentity {
  readonly canonicalPath: readonly string[]
  /** Prevents a journal-owned path from matching a pre-existing file at the same path. */
  readonly ownedFileIdentity: string
}

/**
 * The adapter must invoke openRevision and await it before creating or opening an
 * output object. This callback boundary keeps failed preparation/revision opens
 * from leaving placeholders in a destination namespace.
 */
export interface OutputFileRequest {
  readonly source: OutputCatalogFileIdentity
  readonly sourcePath: readonly string[]
  readonly artifactPath: readonly string[]
  readonly expectedSize: bigint
  readonly parentAdmission?: DirectoryAdmission
  readonly modifiedTime?: CanonicalModifiedTime
  readonly openRevision: (signal: AbortSignal) => Promise<OpenedOutputRevision>
}

export interface OutputFile {
  readonly source: OutputSourceIdentity
  readonly sourcePath: readonly string[]
  readonly artifactPath: readonly string[]
  readonly exactSize: bigint
  readonly parentAdmission?: DirectoryAdmission
  readonly modifiedTime?: CanonicalModifiedTime
}

/** Only a backend may return this value after reopening and validating output. */
export class VerifiedDurableRanges {
  readonly ownership: OutputFileOwnership
  readonly source: OutputSourceIdentity
  readonly #ranges: ByteRangeSet

  constructor(
    ownership: OutputFileOwnership,
    source: OutputSourceIdentity,
    fileSize: bigint,
    ranges: readonly ByteRange[],
  ) {
    this.ownership = snapshotOutputOwnership(ownership)
    this.source = snapshotOutputSource(source)
    this.#ranges = verifiedDurableRangeSet(fileSize, ranges)
  }

  get fileSize(): bigint { return this.#ranges.fileSize }
  get ranges(): readonly ByteRange[] { return this.#ranges.ranges }
  covers(range: ByteRange): boolean { return this.#ranges.covers(range) }
  asRangeSet(): ByteRangeSet { return new ByteRangeSet(this.fileSize, this.ranges) }
}

export interface OutputFileTransaction {
  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void>
  /** Durable sessions report journaled ranges; transient streams retain an empty range set. */
  checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges>
  commit(signal: AbortSignal): Promise<void>
  /** A streaming backend may still retire a failed member when no bytes were emitted. */
  retire(reason: unknown): Promise<FileRetirementDisposition>
  /** Reaches the backend's stable resumable cut without deleting owned output. */
  pause(reason: unknown): Promise<void>
}

export interface BeginOutputFileResult {
  readonly revision: OpenedOutputRevision
  readonly transaction: OutputFileTransaction
  readonly durableRanges: VerifiedDurableRanges
}

/** A materializer owns bytes and checkpoints only; artifact semantics stay in the plan execution. */
export interface OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  beginFile(file: OutputFileRequest, signal: AbortSignal): Promise<BeginOutputFileResult>
}

export interface DirectoryMaterializationRequest {
  readonly source: MaterializationDirectory
  readonly artifactPath: readonly string[]
}

/** Incremental directory authority is legal only for DirectTree and DirectAtomic ZIP. */
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

interface PlanExecutionBase<Plan extends MaterializationPlan> {
  readonly planKind: Plan['kind']
  readonly output: OutputSession
  pause(request: PlanPauseRequest, signal: AbortSignal): Promise<ReceiveLifecycleState>
}

export interface DirectTreeExecution extends PlanExecutionBase<DirectTreePlan> {
  readonly planKind: 'direct-tree'
  readonly directories: IncrementalDirectoryOutput
  settle(
    request: PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}

export interface DirectAtomicExecution extends PlanExecutionBase<DirectAtomicPlan> {
  readonly planKind: 'direct-atomic'
  /** Present only for the legal progressive ZIP form. */
  readonly directories?: IncrementalDirectoryOutput
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
  abortUnopened(
    intent: ReceiveIntent,
    reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  recordSettlementUnknown(
    intent: ReceiveIntent,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>>
}

export class OutputSessionBindingError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'OutputSessionBindingError'
  }
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
  if (intent.plan.kind === 'direct-tree' && !hasDirectoryPort(execution)) {
    throw new OutputSessionBindingError('DirectTree execution requires incremental directory authority')
  }
  if (intent.plan.kind === 'direct-atomic' && intent.artifact.kind === 'zip-archive' &&
      !hasDirectoryPort(execution)) {
    throw new OutputSessionBindingError('DirectAtomic ZIP requires incremental directory authority')
  }
  return execution
}

function hasDirectoryPort(execution: PlanExecution): boolean {
  if (!('directories' in execution) || execution.directories === undefined) return false
  return typeof execution.directories.admitDirectory === 'function' &&
    typeof execution.directories.finalizeDirectory === 'function'
}

export class TransferPauseRequestedError extends Error {
  constructor(message = 'Transfer paused by receiver') {
    super(message)
    this.name = 'TransferPauseRequestedError'
  }
}

/** A finite output-operation budget was exhausted before accepting more state. */
export class OutputBudgetExceededError extends Error {
  readonly budget: string
  readonly limit: bigint
  readonly attempted: bigint

  constructor(budget: string, limit: bigint, attempted: bigint) {
    super(`Output budget ${budget} permits ${limit.toString()}, attempted ${attempted.toString()}`)
    this.name = 'OutputBudgetExceededError'
    this.budget = budget
    this.limit = limit
    this.attempted = attempted
  }
}

export function outputCapabilities(capabilities: OutputCapabilities): OutputCapabilities {
  if ((capabilities.durability !== 'None' &&
       capabilities.durability !== 'ProcessRestart' &&
       capabilities.durability !== 'PowerLoss') ||
      typeof capabilities.randomWrite !== 'boolean' ||
      typeof capabilities.fileFailureIsolation !== 'boolean' ||
      typeof capabilities.modificationTime !== 'boolean') {
    throw new OutputSessionBindingError('output session reported malformed capabilities')
  }
  return Object.freeze({ ...capabilities })
}

export function outputSessionIdentity(identity: OutputSessionIdentity): OutputSessionIdentity {
  return Object.freeze({
    backend: requireIdentityPart(identity.backend, 'output backend', MAXIMUM_OUTPUT_IDENTITY_BYTES),
    outputSessionId: requireIdentityPart(
      identity.outputSessionId,
      'output session',
      MAXIMUM_OUTPUT_IDENTITY_BYTES,
    ),
  })
}

export function snapshotOutputFileRequest(file: OutputFileRequest): OutputFileRequest {
  if (typeof file.expectedSize !== 'bigint' || file.expectedSize < 0n) {
    throw new RangeError('expected output file size must not be negative')
  }
  if (typeof file.openRevision !== 'function') {
    throw new TypeError('output file request requires an authenticated revision callback')
  }
  const sourcePath = snapshotMaterializationPath(file.sourcePath)
  const artifactPath = snapshotMaterializationPath(file.artifactPath)
  if (sourcePath.length === 0 || artifactPath.length === 0) {
    throw new TypeError('output file paths must identify a file')
  }
  return Object.freeze({
    source: snapshotOutputCatalogFileIdentity(file.source),
    sourcePath,
    artifactPath,
    expectedSize: file.expectedSize,
    ...(file.parentAdmission === undefined
      ? {}
      : { parentAdmission: snapshotDirectoryAdmission(file.parentAdmission) }),
    ...(file.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotCanonicalModifiedTime(file.modifiedTime) }),
    openRevision: file.openRevision,
  })
}

export function snapshotOutputFile(file: OutputFile): OutputFile {
  if (typeof file.exactSize !== 'bigint' || file.exactSize < 0n) {
    throw new RangeError('output file size must not be negative')
  }
  const sourcePath = snapshotMaterializationPath(file.sourcePath)
  const artifactPath = snapshotMaterializationPath(file.artifactPath)
  if (sourcePath.length === 0 || artifactPath.length === 0) {
    throw new TypeError('output file paths must identify a file')
  }
  return Object.freeze({
    source: snapshotOutputSource(file.source),
    sourcePath,
    artifactPath,
    exactSize: file.exactSize,
    ...(file.parentAdmission === undefined
      ? {}
      : { parentAdmission: snapshotDirectoryAdmission(file.parentAdmission) }),
    ...(file.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotCanonicalModifiedTime(file.modifiedTime) }),
  })
}

export function snapshotDirectoryMaterializationRequest(
  request: DirectoryMaterializationRequest,
): DirectoryMaterializationRequest {
  return Object.freeze({
    source: snapshotMaterializationDirectory(request.source),
    artifactPath: snapshotMaterializationPath(request.artifactPath),
  })
}

export function snapshotOpenedOutputRevision(revision: OpenedOutputRevision): OpenedOutputRevision {
  if (typeof revision.exactSize !== 'bigint' || revision.exactSize < 0n) {
    throw new TypeError('opened output revision size is invalid')
  }
  return Object.freeze({
    ...snapshotOutputSource(revision),
    exactSize: revision.exactSize,
  })
}

function snapshotOutputCatalogFileIdentity(source: OutputCatalogFileIdentity): OutputCatalogFileIdentity {
  return Object.freeze({
    shareInstance: requireSourceIdentity(source.shareInstance, 'shareInstance'),
    fileId: requireSourceIdentity(source.fileId, 'fileId'),
  })
}

function snapshotOutputSource(source: OutputSourceIdentity): OutputSourceIdentity {
  return Object.freeze({
    ...snapshotOutputCatalogFileIdentity(source),
    fileRevision: requireSourceIdentity(source.fileRevision, 'fileRevision'),
  })
}

function snapshotOutputOwnership(ownership: OutputFileOwnership): OutputFileOwnership {
  const identity = outputSessionIdentity(ownership)
  return Object.freeze({
    ...identity,
    canonicalPath: snapshotMaterializationPath(ownership.canonicalPath),
    ownedFileIdentity: requireIdentityPart(
      ownership.ownedFileIdentity,
      'owned output file',
      MAXIMUM_OWNED_FILE_IDENTITY_BYTES,
    ),
  })
}

function requireIdentityPart(value: string, label: string, maximumBytes: number): string {
  if (typeof value !== 'string' || value.length === 0 ||
      new TextEncoder().encode(value).byteLength > maximumBytes) {
    throw new TypeError(`${label} identity must contain at most ${maximumBytes} bytes`)
  }
  return value
}

function requireSourceIdentity(value: string, label: string): string {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== OUTPUT_CATALOG_IDENTITY_BYTES ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new OutputSessionBindingError(
      `output source ${label} must be a canonical non-zero ${OUTPUT_CATALOG_IDENTITY_BYTES}-byte identity`,
    )
  }
  return value
}

function verifiedDurableRangeSet(fileSize: bigint, ranges: readonly ByteRange[]): ByteRangeSet {
  if (!Array.isArray(ranges) || ranges.length > MAXIMUM_VERIFIED_DURABLE_RANGES) {
    throw new OutputSessionBindingError('output durable ranges exceed their canonical count limit')
  }
  let previousEnd: bigint | undefined
  for (const range of ranges) {
    if (typeof range?.start !== 'bigint' || typeof range.end !== 'bigint' ||
        range.start < 0n || range.start >= range.end || range.end > fileSize ||
        (previousEnd !== undefined && range.start <= previousEnd)) {
      throw new OutputSessionBindingError(
        'output durable ranges must be sorted, non-overlapping, non-adjacent, and within the file',
      )
    }
    previousEnd = range.end
  }
  return new ByteRangeSet(fileSize, ranges)
}

export function needsAttentionFault(fault: Fault): Fault {
  if (!isFault(fault) ||
      (fault.scope !== FaultScope.OutputPause && fault.scope !== FaultScope.SessionTerminal)) {
    throw new TypeError('needs-attention state requires a job-scoped fault')
  }
  return Object.freeze({ ...fault })
}
