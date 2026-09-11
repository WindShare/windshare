import type { StagingExportAuthority } from '../staging-budget/export-authority'
import type { FileCheckpointV2 } from '../persistence/checkpoint'
import type {
  PersistentMaterializationPort,
  PersistentFileRequest, PersistentFileTransactionPort,
} from '../persistent-tree/contracts'
import type { ObjectCapacity } from '../origin-private/object-capacity'
import type { BrowserDeliveryRecordV1, BrowserDeliverySource, BrowserFilePlacement } from './model'

export interface BrowserDirectFileDelivery {
  currentRecord(): BrowserDeliveryRecordV1
  committed(record: BrowserDeliveryRecordV1): void
}

export interface BrowserDeliveryTargetPort extends PersistentMaterializationPort {
  beginDirectFile(request: PersistentFileRequest, delivery: BrowserDirectFileDelivery): Promise<PersistentFileTransactionPort>
  readCheckpoint(fileId: string): Promise<FileCheckpointV2 | undefined>
  verifyStagedTarget(record: BrowserDeliveryRecordV1, content: Blob): Promise<'empty' | 'matching-staged-content'>
}

export interface BrowserDeliveryStageReader {
  readonly blob: Blob
  release(): void
}

export interface BrowserDeliveryStagePort extends PersistentMaterializationPort {
  readCheckpoint(fileId: string): Promise<FileCheckpointV2 | undefined>
  readComplete(checkpoint: FileCheckpointV2): Promise<BrowserDeliveryStageReader>
  removeComplete(checkpoint: FileCheckpointV2): Promise<void>
  discard(source: BrowserDeliverySource, checkpoint?: FileCheckpointV2): Promise<void>
  bindCapacity(source: BrowserDeliverySource, capacity: ObjectCapacity): Promise<void>
}

export interface BrowserDeliveryReservation {
  readonly objectCapacity: ObjectCapacity
  received(verifiedBytes: bigint): Promise<void>
  queueExport(): Promise<void>
  beginExport(authority: StagingExportAuthority): Promise<boolean>
  exportFailed(): Promise<void>
  targetSaved(): Promise<void>
  releaseDeleted(): Promise<void>
  releaseDiscarded(): Promise<void>
  cancelUnused(): Promise<void>
}

export interface BrowserDeliveryPlacementDecision {
  readonly placement: BrowserFilePlacement
  readonly reason: string
}

export interface BrowserDeliveryRuntimeTrace {
  readonly name: 'browser.delivery.runtime'
  readonly operation_id: string
  readonly file_id: string
  readonly transition: 'placement' | 'receiving' | 'copy-started' | 'copy-failed' | 'target-saved' | 'cleanup-failed' | 'cleaned'
    | 'stop-staging-preserved' | 'stop-cleanup-pending' | 'discard-started' | 'discarded' | 'discard-failed'
  readonly placement?: BrowserFilePlacement
  readonly placement_reason?: string
  readonly received_bytes?: bigint
  readonly recoverable_bytes?: bigint
  readonly copy_milliseconds?: number
  readonly failure_name?: string
}
