import type { V2ShareDescriptor } from '../catalog/v2-records'
import type { V2CatalogClient } from '../catalog/v2-client'
import type { V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import type { V2BlockRangeReader } from '../content/v2-broker'
import type { V2RevisionReader } from '../content/v2-session-services'
import {
  DirectZipCatalogSourceV1,
  DirectZipOrderedCoordinatorV1,
  transferDirectZipFileV1,
  type DirectZipIntent,
} from './direct-zip'
import type { SelectionMeasure } from './measure'
import type { DirectResumableZipExecution } from './output-session'

export interface DirectZipJobOrchestration {
  readonly descriptor: V2ShareDescriptor
  readonly catalog: V2CatalogClient
  readonly selection: V2FrozenSelectionPolicy
  readonly intent: DirectZipIntent
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly execution: DirectResumableZipExecution
  readonly maximumNodeClaims: number
  readonly signal: AbortSignal
  readonly observeSelectedFile: (exactSize: bigint) => void
  readonly observeReplayedFile: (exactSize: bigint) => void
  readonly acknowledgeWrite: (bytes: bigint) => void
  readonly completeFile: (exactSize: bigint) => void
  readonly finishMeasure: () => SelectionMeasure
}

/** Keeps the ordered ZIP route outside the generic concurrent file-worker contract. */
export function runDirectZipJob(
  orchestration: DirectZipJobOrchestration,
): Promise<SelectionMeasure> {
  const source = new DirectZipCatalogSourceV1({
    catalog: orchestration.catalog,
    descriptor: orchestration.descriptor,
    selection: orchestration.selection,
    intent: orchestration.intent,
    maximumNodeClaims: orchestration.maximumNodeClaims,
  })
  return new DirectZipOrderedCoordinatorV1({
    source,
    output: orchestration.execution.ordered,
    signal: orchestration.signal,
    observeSelectedFile: orchestration.observeSelectedFile,
    observeReplayedFile: orchestration.observeReplayedFile,
    transferFile: file => transferDirectZipFileV1({
      descriptor: orchestration.descriptor,
      revisions: orchestration.revisions,
      broker: orchestration.broker,
      output: orchestration.execution.output,
      signal: orchestration.signal,
      onWriteAcknowledged: orchestration.acknowledgeWrite,
      onComplete: orchestration.completeFile,
    }, file),
    finishMeasure: orchestration.finishMeasure,
  }).run()
}
