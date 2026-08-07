import type { V2CatalogClient } from '../catalog/v2-client'
import type {
  V2CatalogEntry,
  V2CatalogModifiedTime,
  V2ShareDescriptor,
} from '../catalog/v2-records'
import type { V2SelectionPolicy } from '../catalog/v2-selection'
import type { V2BlockRangeReader, V2ContentLaneStatus } from '../content/v2-broker'
import type { V2RevisionReader } from '../content/v2-session-services'
import type { TransferIntent, TransferIntentDraft, TransferTraceListener } from './intent'
import type { SelectionMeasure } from './measure'
import type { JobOutcome } from './outcome'
import type { V2OutputSettlement } from './settlement/v2-output'
import {
  type DirectoryAdmission,
  type V2OutputAuthority,
} from './output-session'
import { MAXIMUM_OUTPUT_DIRECTORY_ADMISSIONS } from './directory-admission-ledger'

export const V2_MAXIMUM_CONCURRENT_FILES = 4
export const V2_MAXIMUM_CONCURRENT_DIRECTORIES = 4
export const V2_MAXIMUM_PENDING_DIRECTORIES = 64
export const V2_MAXIMUM_PENDING_FILES = 64
export const V2_MAXIMUM_PENDING_FILE_METADATA_BYTES = 64n * 1024n * 1024n
export const V2_MAXIMUM_CATALOG_NODE_CLAIMS = 1_000_000
export const V2_MAXIMUM_DIRECTORY_ADMISSIONS = MAXIMUM_OUTPUT_DIRECTORY_ADMISSIONS

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
  readonly completedFiles: number
  readonly completedBytes: bigint
  readonly fileErrors: number
  readonly selectionErrors: number
  readonly contentLanes: number
  readonly discovery: SelectionMeasure['discovery']
  readonly failedDirectories: number
  readonly partial: boolean
  readonly transferJobId: string
  readonly outputSessionId?: string
}

export interface TransferJobOptions {
  readonly descriptor: V2ShareDescriptor
  readonly catalog: V2CatalogClient
  readonly selection: V2SelectionPolicy
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly lanes: V2ContentLaneStatus
  readonly output: V2OutputAuthority
  readonly intent?: TransferIntent | TransferIntentDraft
  readonly transferJobId?: string
  readonly onProgress?: (progress: TransferProgress) => void
  readonly onMeasure?: (measure: SelectionMeasure) => void
  readonly onTrace?: TransferTraceListener
  /** Resolves the active ProtocolSession generation at the instant each trace is emitted. */
  readonly protocolSessionId?: () => string
  readonly maximumConcurrentFiles?: number
  readonly maximumConcurrentDirectories?: number
  readonly maximumPendingFiles?: number
  readonly maximumPendingFileMetadataBytes?: bigint
  readonly maximumNodeClaims?: number
  readonly maximumDirectoryAdmissions?: number
  /** One terminal collaborator may consume at most this bounded settlement interval. */
  readonly outputSettlementTimeoutMilliseconds?: number
}

export interface DirectoryCursor {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly selected: boolean
  readonly modifiedTime?: V2CatalogModifiedTime
}

export interface DirectoryWork {
  readonly cursor: DirectoryCursor
  /** Lazily materializes the parent chain only when this subtree contains selected output. */
  readonly materializeParent: () => Promise<DirectoryAdmission>
}

export interface PendingFile {
  readonly entry: Extract<V2CatalogEntry, { kind: 'file' }>
  readonly path: readonly string[]
  readonly parentAdmission: DirectoryAdmission
  readonly modifiedTime?: V2CatalogModifiedTime
  /** Producer trace publication must happen before a woken worker can start. */
  readonly ready: Promise<void>
}

export interface TransferJobResult {
  readonly outcome: JobOutcome
  readonly settlement: V2OutputSettlement
  readonly measure: SelectionMeasure
  readonly abortReason?: unknown
  readonly transferJobId: string
}
