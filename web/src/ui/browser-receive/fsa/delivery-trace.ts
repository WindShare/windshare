import type { BrowserDeliveryRuntimeTrace } from '../../../output/browser-delivery/ports'
import { emitOutputTrace, outputTraceEvent, type OutputDiagnosticsPorts } from '../../../output/diagnostics'
import type { TransferJobOptions } from '../../../transfer/job/contract'

export function browserFolderDeliveryTrace(diagnostics: OutputDiagnosticsPorts | undefined) {
  return (event: BrowserDeliveryRuntimeTrace): void => {
    emitOutputTrace(diagnostics?.trace, () => outputTraceEvent('browser_delivery', {
      operation_id: event.operation_id, file_id: event.file_id, transition: event.transition,
      ...(event.placement === undefined ? {} : { placement: event.placement }),
      ...(event.placement_reason === undefined ? {} : { placement_reason: event.placement_reason }),
      ...(event.received_bytes === undefined ? {} : { received_bytes: event.received_bytes.toString() }),
      ...(event.recoverable_bytes === undefined ? {} : { recoverable_bytes: event.recoverable_bytes.toString() }),
      ...(event.copy_milliseconds === undefined ? {} : { copy_milliseconds: event.copy_milliseconds }),
      ...(event.failure_name === undefined ? {} : { failure_name: event.failure_name }),
    }))
  }
}

export function observeBrowserFolderCheckpoint(diagnostics: OutputDiagnosticsPorts | undefined,
  event: Parameters<NonNullable<TransferJobOptions['onCheckpointObservation']>>[0]): void {
  if (event.stage === 'pending') return
  const checkpointStage = event.stage
  emitOutputTrace(diagnostics?.trace, () => outputTraceEvent('browser_delivery', {
    operation_id: event.operationId, file_id: event.fileId, transition: 'checkpoint',
    object_id: event.objectId, checkpoint_stage: checkpointStage,
    pending_bytes: event.pendingBytes.toString(), recoverable_bytes: event.durableBytes.toString(),
    at_milliseconds: event.atMilliseconds,
    ...(event.lastCheckpointMilliseconds === undefined ? {} : { last_checkpoint_milliseconds: event.lastCheckpointMilliseconds }),
    ...(event.durationMilliseconds === undefined ? {} : { checkpoint_milliseconds: event.durationMilliseconds }),
  }))
}
