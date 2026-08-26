import type { V2CatalogClient } from '../../catalog/v2-client'
import type {
  V2CatalogEntry,
  V2CatalogModifiedTime,
  V2ShareDescriptor,
} from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy, V2SelectionPolicy } from '../../catalog/v2-selection'
import type { V2BlockRangeReader, V2ContentLaneStatus } from '../../content/v2-broker'
import type { V2RevisionReader } from '../../content/v2-session-services'
import type { IncidentScopeHandle } from '../../diagnostics/incident'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type { CompatibleNameRepairSummary } from '../../output/file-system-access/compatible-name/model'
import type { RecoverySummary } from '../../output/file-system-access/recovery-summary'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type {
  CanonicalModifiedTime,
  DirectoryAdmission,
  DirectoryAdmissionLayout,
} from '../directory-admission'
import type { ReceiveIntent } from '../intent'
import type { SelectionMeasure } from '../measure'
import type { TransferWorkerSettlement } from '../outcome'
import type { ClassifiedTransferFailure } from './failures'
import type {
  LogicalArtifactPath,
  MaterializationRootRelativePath,
  SourceAuthenticationPath,
} from './coordinate/direct-tree'
import type {
  DurabilityLevel,
  ExactPreparationEvidence,
  V2PlanExecutionAuthority,
} from '../output-session'
import { MAXIMUM_OPEN_OUTPUT_FILES } from '../output-file-contract'
import { MAX_DIRECTORY_ADMISSIONS } from '../directory-admission-ledger'
import type { V2OutputSettlementDeadline } from '../settlement/v2-output'
import type { WorkerFamilyFailureSource } from '../worker-family/supervisor'
import type {
  V2RevisionCapacityPolicyOptions,
  V2RevisionCapacitySurface,
} from '../revision-capacity/public'

/** Absolute process-safety ceiling; concrete output policy is bound after execution admission. */
export const V2_MAXIMUM_CONCURRENT_FILES = MAXIMUM_OPEN_OUTPUT_FILES
export const V2_MAXIMUM_CONCURRENT_DIRECTORIES = 4
export const V2_MAXIMUM_PENDING_DIRECTORIES = 64
export const V2_MAXIMUM_PENDING_FILES = 64
export const V2_MAXIMUM_PENDING_FILE_METADATA_BYTES = 64n * 1024n * 1024n
export const V2_MAXIMUM_CATALOG_NODE_CLAIMS = 1_000_000
export const V2_MAXIMUM_DIRECTORY_ADMISSIONS = MAX_DIRECTORY_ADMISSIONS

export class V2CatalogTraversalError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2CatalogTraversalError'
  }
}

/** A failure confined to one catalog subtree; global authority faults use the base type. */
export class V2DirectoryTraversalError extends V2CatalogTraversalError {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2DirectoryTraversalError'
  }
}

export class V2OutputPausedError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2OutputPausedError'
  }
}

/** Releases authenticated directory identities as soon as their discovery closes. */
export class V2DirectoryAncestry {
  readonly #active = new Set<string>()
  #maximumDepth = 0

  get depth(): number { return this.#active.size }
  get maximumDepth(): number { return this.#maximumDepth }

  enter(directoryIdText: string): () => void {
    if (this.#active.has(directoryIdText)) {
      throw new V2CatalogTraversalError('Catalog traversal revisited an ancestor identity')
    }
    this.#active.add(directoryIdText)
    this.#maximumDepth = Math.max(this.#maximumDepth, this.#active.size)
    let active = true
    return () => {
      if (!active || !this.#active.delete(directoryIdText)) {
        throw new Error('Catalog traversal ancestry ownership was released twice')
      }
      active = false
    }
  }
}

export interface TransferProgress {
  readonly discoveredFiles: number
  readonly discoveredBytes: bigint
  readonly writtenBytes: bigint
  readonly recoverableBytes: bigint
  readonly completedFiles: number
  readonly completedBytes: bigint
  readonly fileErrors: number
  readonly selectionErrors: number
  readonly contentLanes: number
  readonly discovery: SelectionMeasure['discovery']
  readonly failedDirectories: number
  readonly capacityWaitingFiles: number
  readonly capacityAccumulatedWaitMilliseconds: number
  readonly capacityWaitAttempts: number
  readonly capacityWaitVisible: boolean
  readonly partial: boolean
  readonly transferJobId: string
  readonly outputSessionId?: string
}

export type ArtifactLayoutClass = DirectoryAdmissionLayout | 'original-file'
export const MATERIALIZATION_FAILURE_REASONS = Object.freeze([
  'file-open-failed',
  'source-revision-changed',
  'content-read-failed',
  'output-write-failed',
  'output-commit-failed',
  'directory-finalize-failed',
] as const)

export type MaterializationFailureReason =
  (typeof MATERIALIZATION_FAILURE_REASONS)[number]

export type TransferTraceEvent =
  | Readonly<{
      name: 'receive_transition'
      transition: 'intent_frozen'
      artifactKind: ReceiveIntent['artifact']['kind']
      layoutClass: ArtifactLayoutClass
      planKind: ReceiveIntent['plan']['kind']
    }>
  | Readonly<{
      name: 'receive_transition'
      transition: 'directory_admitted'
      admittedDirectoryCount: bigint
      layoutClass: DirectoryAdmissionLayout
    }>
  | Readonly<{
      name: 'receive_transition'
      transition: 'materialization_started'
      planKind: ReceiveIntent['plan']['kind']
    }>
  | Readonly<{
      name: 'receive_transition'
      transition: 'materialization_failed'
      planKind: ReceiveIntent['plan']['kind']
      materializationFailureReason: MaterializationFailureReason
      completedFileCount: bigint
      completedBytes: bigint
    }>
  | Readonly<{
      name: 'receive_transition'
      transition: 'materialization_completed'
      entryCount: bigint
      fileCount: bigint
      directoryCount: bigint
      rawBytes: bigint
    }>
  | Readonly<{
      name: 'receive_transition'
      transition: 'tree_finalized'
      outcome: 'published' | 'partial_directory' | 'discarded'
      successCount: bigint
      failureCount: bigint
    }>
  | Readonly<{
      name: 'receive_transition'
      transition: 'worker_consequence_observed'
      workerFamily: 'discovery' | 'prepared-files'
      failureSource: WorkerFamilyFailureSource
      operationId: string
      transferJobId: string
      protocolSessionId?: string
      protocolGeneration?: number
      outputSessionId?: string
    }>
  | Readonly<{
      name: 'receive_transition'
      transition:
        | 'capacity_retry_scheduled'
        | 'capacity_retry_succeeded'
        | 'capacity_wait_budget_paused'
        | 'capacity_wait_cancelled'
        | 'capacity_generation_replaced'
      capacityWaitId: string
      capacitySurface: V2RevisionCapacitySurface
      receiveOperationId: string
      transferJobId: string
      protocolSessionId: string
      protocolOperationId: string
      attempt: bigint
      senderHintMilliseconds: number
      jitterMilliseconds: number
      delayMilliseconds: number
      accumulatedWaitMilliseconds: number
      activeWaiters: number
    }>
  | Readonly<{
      name: 'transfer_progress'
      discoveredFiles: bigint
      discoveredBytes: bigint
      writtenBytes: bigint
      completedFiles: bigint
      completedBytes: bigint
      fileErrors: bigint
      selectionErrors: bigint
      failedDirectories: bigint
      capacityWaitingFiles: bigint
      capacityAccumulatedWaitMilliseconds: number
      capacityWaitAttempts: bigint
      capacityWaitVisible: boolean
      contentLanes: number
      discovery: SelectionMeasure['discovery']
      partial: boolean
    }>

export interface TransferJobOptions {
  readonly descriptor: V2ShareDescriptor
  readonly catalog: V2CatalogClient
  readonly selection: V2FrozenSelectionPolicy | V2SelectionPolicy
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly revisionCapacity: V2RevisionCapacityPolicyOptions
  readonly lanes: V2ContentLaneStatus
  readonly plans: V2PlanExecutionAuthority
  readonly intent: ReceiveIntent
  readonly transferJobId?: string
  readonly protocol?: Readonly<{
    readonly sessionId: string
    readonly generation: number
  }>
  readonly onProgress?: (progress: TransferProgress) => void
  readonly onMeasure?: (measure: SelectionMeasure) => void
  readonly trace?: DomainTraceSource<TransferTraceEvent>
  readonly incidentScope?: IncidentScopeHandle
  readonly maximumConcurrentFiles?: number
  readonly maximumConcurrentDirectories?: number
  readonly maximumPendingFiles?: number
  readonly maximumPendingFileMetadataBytes?: bigint
  readonly maximumNodeClaims?: number
  readonly maximumDirectoryAdmissions?: number
  /** One terminal collaborator may consume at most this bounded settlement interval. */
  readonly outputSettlementTimeoutMilliseconds?: number
  /** Injectable deadline ownership keeps terminal-cut concurrency tests independent of wall time. */
  readonly outputSettlementDeadline?: V2OutputSettlementDeadline
}

export interface DirectoryCursor {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly selected: boolean
  readonly modifiedTime?: V2CatalogModifiedTime
}

interface AuthenticatedDirectoryBase {
  readonly directoryId: string
  readonly generation: string
  readonly sourceAuthenticationPath: SourceAuthenticationPath
  readonly logicalArtifactPath: LogicalArtifactPath
  readonly modifiedTime?: CanonicalModifiedTime
}

export type AuthenticatedDirectory =
  | (AuthenticatedDirectoryBase & Readonly<{
      kind: 'reference'
    }>)
  | (AuthenticatedDirectoryBase & Readonly<{
      kind: 'materialized'
      materializationRelativePath: MaterializationRootRelativePath
      admission: DirectoryAdmission
    }>)

/**
 * Exact committed-generation authority for the logical children of one directory.
 * Output backends may consult it only while evaluating an activated compatible-name candidate.
 */
export interface AuthenticatedLogicalSiblingMembership {
  readonly directoryId: string
  readonly generation: string
  hasCommittedName(candidate: string): Promise<boolean>
}

export interface DirectoryWork {
  readonly cursor: DirectoryCursor
  /** Lazily records or materializes the parent chain only when selected output requires it. */
  readonly materializeParent: (role?: 'selected' | 'ancestor') => Promise<AuthenticatedDirectory>
}

export interface PendingFile {
  readonly entry: Extract<V2CatalogEntry, { kind: 'file' }>
  readonly sourceAuthenticationPath: SourceAuthenticationPath
  readonly logicalArtifactPath: LogicalArtifactPath
  readonly materializationRelativePath: MaterializationRootRelativePath
  readonly parent: AuthenticatedDirectory
  readonly modifiedTime?: V2CatalogModifiedTime
  /** Producer trace publication must happen before a woken worker can start. */
  readonly ready: Promise<void>
}

export interface TransferJobResult {
  readonly worker: TransferWorkerSettlement
  readonly lifecycle: ReceiveLifecycleState
  /** Repair qualifies output independently, so ordinary lifecycle kinds remain canonical. */
  readonly repairSummary?: CompatibleNameRepairSummary
  /** Choice costs are exposed only after the resumable lifecycle authenticates its checkpoint set. */
  readonly recoverySummary?: RecoverySummary
  readonly measure: SelectionMeasure
  readonly abortReason?: unknown
  readonly failureTrigger?: ClassifiedTransferFailure
  readonly transferJobId: string
  readonly intent: ReceiveIntent
  readonly outputDurability?: DurabilityLevel
  readonly preparation?: ExactPreparationEvidence
}
