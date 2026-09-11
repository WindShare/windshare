import type { StagingExportAuthority } from '../staging-budget/export-authority'
import type { FileCheckpointV2 } from '../persistence/checkpoint'
import type {
  PersistentMaterializationPort,
} from '../persistent-tree/contracts'
import type { ObjectCapacity } from '../origin-private/object-capacity'
import type { BrowserDeliverySource, BrowserFilePlacement } from './model'

export interface BrowserDeliveryTargetPort extends PersistentMaterializationPort {
  readCheckpoint(fileId: string): Promise<FileCheckpointV2 | undefined>
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
  readonly placement?: BrowserFilePlacement
  readonly placement_reason?: string
  readonly received_bytes?: bigint
  readonly recoverable_bytes?: bigint
  readonly copy_milliseconds?: number
  readonly failure_name?: string
}
