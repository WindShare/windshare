import { type MaterializationFailureReason } from '../failure/materialization'
export type FileTransferSettlement =
  | Readonly<{ kind: 'completed'; exactSize: bigint }>
  | Readonly<{ kind: 'paused'; retainedBytes: bigint }>
  | Readonly<{ kind: 'unconfirmed' }>

import type { CatalogClient } from '../../catalog/client'
import type {
  CatalogEntry,
  CatalogModifiedTime,
  ShareDescriptor,
} from '../../catalog/records'
import type { FrozenSelectionPolicy, SelectionPolicy } from '../../catalog/selection'
import type { BlockRangeReader, ContentLaneStatus } from '../../content/broker'
import type { RevisionReader } from '../../content/session-services'
import type { IncidentScopeHandle } from '../../diagnostics/incident'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type {
  CompatibleNameRepairSummary,
} from '../lifecycle/name-repair'
import type {
  RecoverySummary,
} from '../lifecycle/recovery-summary'
import type { ReceiveLifecycleState } from '../lifecycle/state'
import type {
  CanonicalModifiedTime,
  DirectoryAdmission,
  DirectoryAdmissionLayout,
} from '../directory-admission'
import type { ReceiveIntent } from '../intent'
import type { SelectionMeasure } from '../measure'
import type { TransferWorkerSettlement } from '../outcome'
import type {
  ClassifiedTransferFailure,
} from '../failure/classification'
import type {
  LogicalArtifactPath,
  MaterializationRootRelativePath,
  SourceAuthenticationPath,
} from './coordinate/direct-tree'
import type {
  DurabilityLevel,
  ExactPreparationEvidence,
  PlanExecutionAuthority,
} from '../output/session'
import { MAXIMUM_OPEN_OUTPUT_FILES } from '../output/file'
import { MAX_DIRECTORY_ADMISSIONS } from '../directory-admission-ledger'
import type { OutputSettlementDeadline } from '../settlement/output'
import type { WorkerFamilyFailureSource } from '../worker-family/supervisor'
import type {
  RevisionCapacityPolicyOptions,
  RevisionCapacitySurface,
} from '../revision-capacity/public'

/** Absolute process-safety ceiling; concrete output policy is bound after execution admission. */
export const MAXIMUM_CONCURRENT_FILES = MAXIMUM_OPEN_OUTPUT_FILES
export const MAXIMUM_CONCURRENT_DIRECTORIES = 4
export const MAXIMUM_PENDING_DIRECTORIES = 64
export const MAXIMUM_PENDING_FILES = 64
export const MAXIMUM_PENDING_FILE_METADATA_BYTES = 64n * 1024n * 1024n
export const MAXIMUM_CATALOG_NODE_CLAIMS = 1_000_000
export const MAXIMUM_DIRECTORY_ADMISSIONS = MAX_DIRECTORY_ADMISSIONS

export class CatalogTraversalError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'CatalogTraversalError'
  }
}

/** A failure confined to one catalog subtree; global authority faults use the base type. */
export class DirectoryTraversalError extends CatalogTraversalError {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'DirectoryTraversalError'
  }
}

export class OutputPausedError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'OutputPausedError'
  }
}

/** Releases authenticated directory identities as soon as their discovery closes. */
export class DirectoryAncestry {
  readonly #active = new Set<string>()
  #maximumDepth = 0

  get depth(): number { return this.#active.size }
  get maximumDepth(): number { return this.#maximumDepth }

  enter(directoryIdText: string): () => void {
    if (this.#active.has(directoryIdText)) {
      throw new CatalogTraversalError('Catalog traversal revisited an ancestor identity')
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
  /** Unique fragment payload per upstream attempt, including sealed-object overhead and retried attempts. */
  readonly receivedObjectBytes: bigint
  readonly discoveredFiles: number
  readonly discoveredBytes: bigint
  readonly phase: 'receiving' | 'finishing'
  /** Accepted output payload, including authenticated retained coverage; not restart durability. */
  readonly materializedBytes: bigint
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

export type TransferTraceEvent =
  | (Readonly<{
      name: 'receive_transition'
      transition: 'discovery_scheduling'
      operationId: string
      transferJobId: string
    }> & import('../discovery/queue').DiscoverySchedulingObservation)
  | Readonly<{
      name: 'receive_transition'
      transition: 'download_connectivity'
      transferJobId: string
      connectivity: import('../../diagnostics/trace/transfer-payload').DownloadConnectivitySnapshot
    }>
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
      capacitySurface: RevisionCapacitySurface
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
      sourceOmittedEntries: bigint
      operationId: string
      transferJobId: string
      discoveredFiles: bigint
      discoveredBytes: bigint
      receivedObjectBytes: bigint
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
  readonly checkpointClock?: import('../checkpoint/controller').CheckpointClock
  readonly onCheckpointObservation?: (event: import('../checkpoint/controller').CheckpointObservation &
    Readonly<{ operationId: string; transferJobId: string; fileId: string }>) => void
  readonly descriptor: ShareDescriptor
  readonly catalog: CatalogClient
  readonly selection: FrozenSelectionPolicy | SelectionPolicy
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly revisionCapacity: RevisionCapacityPolicyOptions
  readonly lanes: ContentLaneStatus
  readonly plans: PlanExecutionAuthority
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
  readonly maximumPendingGenerations?: number
  readonly maximumPendingGenerationMetadataBytes?: bigint
  readonly maximumPendingFiles?: number
  readonly maximumPendingFileMetadataBytes?: bigint
  readonly maximumNodeClaims?: number
  readonly maximumDirectoryAdmissions?: number
  /** One terminal collaborator may consume at most this bounded settlement interval. */
  readonly outputSettlementTimeoutMilliseconds?: number
  /** Injectable deadline ownership keeps terminal-cut concurrency tests independent of wall time. */
  readonly outputSettlementDeadline?: OutputSettlementDeadline
}

export interface DirectoryCursor {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly selected: boolean
  readonly modifiedTime?: CatalogModifiedTime
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
  readonly entry: Extract<CatalogEntry, { kind: 'file' }>
  readonly sourceAuthenticationPath: SourceAuthenticationPath
  readonly logicalArtifactPath: LogicalArtifactPath
  readonly materializationRelativePath: MaterializationRootRelativePath
  readonly parent: AuthenticatedDirectory
  readonly modifiedTime?: CatalogModifiedTime
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
