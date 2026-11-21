import { createOperationID } from '../../transfer/intent'
import type { OutputDiagnosticsPorts } from '../diagnostics'
import { acquireArtifactReader } from '../origin-private/export-readers'
import { buildCompleteZipBlob, exportCompleteZipEntries } from '../progressive-zip/partial-export'
import type { RetainedZipPartialReader } from '../resume/reopen/partial-zip-continuation'
import {
  partialExportSuggestedName,
  type PartialExportDestination,
} from '../capability/partial-export'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
  createWindowBrowserHandoffPublisher,
} from './browser-download'
export {
  partialExportSuggestedName,
  preparePartialExport,
  supportsPartialExport,
  supportsPartialExportHandoff,
  supportsPartialExportPicker,
  type PartialExportDestination,
} from '../capability/partial-export'

export async function saveProgressivePartial(
  reader: RetainedZipPartialReader,
  destination: PartialExportDestination,
  signal: AbortSignal,
  diagnostics?: OutputDiagnosticsPorts,
  options?: Readonly<{ detachMaxBytes?: bigint; maxEntries?: number }>,
): Promise<void> {
  const lease = await acquireArtifactReader(reader.object.operationId)
  let handoffOwnsLease = false
  try {
    signal.throwIfAborted()
    const source = await reader.handle.getFile()
    if (destination.kind === 'file-handle') {
      const output = await destination.handle.createWritable()
      await exportCompleteZipEntries({
        source, output, signal,
        entries: () => reader.completeEntries(),
      })
      return
    }
    const built = await buildCompleteZipBlob({
      source, signal,
      entries: () => reader.completeEntries(),
      Blob: destination.windowPort.Blob,
      ...(options?.detachMaxBytes === undefined ? {} : { detachMaxBytes: options.detachMaxBytes }),
      ...(options?.maxEntries === undefined ? {} : { maxEntries: options.maxEntries }),
    })
    signal.throwIfAborted()
    if (built.detached) {
      // Payloads are detached from the mutable OPFS file, so cleanup and resume need not wait for the URL lease.
      lease.release()
    }
    const publisher = createWindowBrowserHandoffPublisher(destination.windowPort, undefined, diagnostics)
    publisher.handoffWithLease({
      context: {
        attemptKind: 'partial',
        operationId: reader.object.operationId,
        attemptId: createOperationID(),
      },
      source: built.blob,
      exactBytes: built.exactBytes,
      suggestedName: partialExportSuggestedName(destination.suggestedName),
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
      ...(built.detached ? {} : { onSourceReleased: () => lease.release() }),
    })
    handoffOwnsLease = !built.detached
  } finally {
    if (!handoffOwnsLease) lease.release()
  }
}
