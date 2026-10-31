import type { ShareDescriptor } from '../catalog/records'
import type { CatalogClient } from '../catalog/client'
import type { FrozenSelectionPolicy } from '../catalog/selection'
import type { BlockRangeReader } from '../content/broker'
import type { RevisionReader } from '../content/session-services'
import {
  DirectZipCatalogSourceV1,
  DirectZipOrderedCoordinatorV1,
  transferDirectZipFileV1,
  type DirectZipIntent,
} from './direct-zip'
import type { SelectionMeasure } from './measure'
import type { DirectResumableZipExecution } from './output/session'
import type { TransferProgressLedger } from './progress/ledger'

interface DirectZipProgressObservers {
  readonly observeReplayedFile: (exactSize: bigint) => void
  readonly observeMaterializedFile: (fileId: string, bytes: bigint) => void
  readonly acknowledgeWrite: (bytes: bigint) => void
  readonly completeFile: (fileId: string, exactSize: bigint) => void
}

export interface DirectZipJobOrchestration extends DirectZipProgressObservers {
  readonly descriptor: ShareDescriptor
  readonly catalog: CatalogClient
  readonly selection: FrozenSelectionPolicy
  readonly intent: DirectZipIntent
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly execution: DirectResumableZipExecution
  readonly maximumNodeClaims: number
  readonly signal: AbortSignal
  readonly observeSelectedFile: (exactSize: bigint) => void
  readonly observeSourceOmissions: (count: bigint) => void
  readonly observeDiscovery: (event: import('./discovery/queue').DiscoverySchedulingObservation) => void
  readonly finishMeasure: () => SelectionMeasure
}

/** Completed members replace their live coverage, while replay never counts as a new write. */
export function createDirectZipProgressObservers(
  progress: Pick<TransferProgressLedger, 'completeFile' | 'observeMaterializedFile' | 'acknowledgeWrite'>,
  emitProgress: () => void,
): DirectZipProgressObservers {
  return {
    observeReplayedFile: exactSize => {
      progress.completeFile(exactSize)
      emitProgress()
    },
    observeMaterializedFile: (fileId, bytes) => {
      progress.observeMaterializedFile(fileId, bytes)
      emitProgress()
    },
    acknowledgeWrite: bytes => {
      progress.acknowledgeWrite(bytes)
      emitProgress()
    },
    completeFile: (fileId, exactSize) => {
      progress.completeFile(exactSize, fileId)
      emitProgress()
    },
  }
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
    observeSourceOmissions: orchestration.observeSourceOmissions,
  })
  return new DirectZipOrderedCoordinatorV1({
    source,
    output: orchestration.execution.ordered,
    signal: orchestration.signal,
    observeSelectedFile: orchestration.observeSelectedFile,
    observeReplayedFile: orchestration.observeReplayedFile,
    transferFile: (file, signal) => {
      let materializedBytes = 0n
      return transferDirectZipFileV1({
        descriptor: orchestration.descriptor,
        revisions: orchestration.revisions,
        broker: orchestration.broker,
        output: orchestration.execution.output,
        signal,
        onInitialDurable: bytes => {
          materializedBytes = bytes
          orchestration.observeMaterializedFile(file.fileId, materializedBytes)
        },
        onWriteAcknowledged: bytes => {
          materializedBytes += bytes
          orchestration.observeMaterializedFile(file.fileId, materializedBytes)
          orchestration.acknowledgeWrite(bytes)
        },
        onComplete: exactSize => orchestration.completeFile(file.fileId, exactSize),
      }, file).finally(() => {
        // A retired writer cannot attest live coverage; retained checkpoints own
        // partial-file progress again after Pause or failed output settlement.
        orchestration.observeMaterializedFile(file.fileId, 0n)
      })
    },
    observeDiscovery: orchestration.observeDiscovery,
    finishMeasure: orchestration.finishMeasure,
  }).run()
}
