import { snapshotContentAvailability, type ContentAvailability } from '../../../transfer/output/content-availability'
import type { TaskCheckpoint, TaskEntry } from './model'

/** Called against a committed entry after the archive owner has verified its local object. */
export function taskEntryContent(entry: TaskEntry, checkpoint: TaskCheckpoint): ContentAvailability {
  if (entry.kind !== 'file' || entry.revision === undefined || entry.zipLayout === undefined) {
    throw new TypeError('Reusable ZIP content requires an opened file and its allocated payload span')
  }
  if (entry.zipLayout.endOffset > checkpoint.allocatedLength ||
      entry.ranges.some(range => entry.zipLayout!.payloadOffset + range.end > checkpoint.physicalLength)) {
    throw new TypeError('ZIP content lies outside its committed object')
  }
  const valid = entry.revisionFailure === undefined
  return snapshotContentAvailability({
    source: { shareInstance: entry.source.shareInstance, fileId: entry.revision.fileId,
      fileRevision: entry.revision.fileRevision },
    exactSize: entry.revision.exactSize,
    replica: valid ? {
      backend: 'browser-origin-private-workspace', operationId: checkpoint.object.operationId,
      objectId: checkpoint.object.objectId, checkpointId: entry.entryId,
      generation: checkpoint.generation, payloadOffset: entry.zipLayout.payloadOffset,
    } : null,
    ranges: valid ? entry.ranges : [],
  })
}
