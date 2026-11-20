import { byteRange } from '../../content/geometry'
import { sourceMetadataFields } from '../../content/revision-comparison'
import {
  observePerformance, performanceElapsedMilliseconds, performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from '../../diagnostics/performance/performance-summary'
import { beginPerformanceRevisionOpen } from '../../diagnostics/performance/performance-runtime-observations'
import {
  snapshotOpenedOutputRevision, type OpenedOutputRevision, type OutputFileOwnership, type OutputFileRequest,
} from '../../transfer/output/session'
import type { OpenedFileRevision, PersistentFileTransactionPort } from '../persistent-tree/contracts'
import { verifiedPersistentRanges } from './persistent-file-transaction'

export function persistentFileRevision(
  request: OutputFileRequest, signal: AbortSignal, performance: PerformanceSummaryObservations | undefined,
) {
  const queuedAt = performanceNowMilliseconds(performance)
  let invoked = false
  let opened: OpenedOutputRevision | undefined

  function catalogRevision(value: OpenedOutputRevision): OpenedOutputRevision {
    const revision = snapshotOpenedOutputRevision(value)
    if (revision.shareInstance !== request.source.shareInstance ||
        revision.fileId !== request.source.fileId || revision.exactSize !== request.expectedSize) {
      throw new TypeError('persistent revision does not match the requested catalog file')
    }
    return revision
  }

  return {
    async openRevision(): Promise<OpenedFileRevision> {
      if (invoked) throw new TypeError('persistent materializer invoked revision authority more than once')
      invoked = true
      observePerformance(performance, summary => summary.markMilestone('first_content_request'))
      const wait = performanceElapsedMilliseconds(queuedAt, performanceNowMilliseconds(performance))
      const observation = beginPerformanceRevisionOpen(performance, wait)
      try {
        opened = catalogRevision(await request.openRevision(signal))
        return Object.freeze({
          fileId: opened.fileId, fileRevision: opened.fileRevision, ...sourceMetadataFields(opened), exactSize: opened.exactSize,
        })
      } finally { observation?.finish(opened !== undefined) }
    },

    bind(transaction: PersistentFileTransactionPort, ownership: OutputFileOwnership, allowLocal: boolean) {
      const actual = transaction.revision
      const replica = transaction.contentReplica
      // Empty ranges also cover a zero-byte file, even on a newly reserved handle.
      if (opened === undefined && (invoked || !allowLocal || replica === null ||
          replica.objectId !== (transaction.checkpointObjectId ?? transaction.ownedObjectId))) {
        throw new TypeError('persistent materializer bypassed authenticated revision authority')
      }
      const revision = opened ?? catalogRevision({ ...actual, shareInstance: request.source.shareInstance })
      if (actual.fileId !== revision.fileId || actual.fileRevision !== revision.fileRevision ||
          actual.exactSize !== revision.exactSize) {
        throw new TypeError('persistent transaction returned another authenticated revision')
      }
      const durableRanges = verifiedPersistentRanges(ownership, revision, transaction.initialDurableRanges, replica)
      if (opened === undefined && !durableRanges.covers(byteRange(0n, revision.exactSize))) {
        throw new TypeError('persistent materializer bypassed authenticated revision authority')
      }
      return { revision, durableRanges }
    },
  }
}
