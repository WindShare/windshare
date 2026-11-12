import { compareRevisionMetadata } from '../../../content/revision-comparison'
import type { FileCheckpointV2 } from '../../persistence/checkpoint'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from '../model'
import { snapshotBrowserDeliveryRecord, validateBrowserDeliveryRecord } from '../records'

/** Synchronize only after the storage layer has durably admitted the current source. */
export function admitBrowserDeliverySource(
  policy: BrowserSavePolicyV1, input: BrowserDeliveryRecordV1, checkpoint: FileCheckpointV2,
): BrowserDeliveryRecordV1 {
  const previous = validateBrowserDeliveryRecord(policy, input)
  const next = snapshotBrowserDeliveryRecord(policy, {
    ...previous, generation: previous.generation + 1n,
    source: { ...previous.source, fileRevision: checkpoint.fileRevision },
    state: { kind: 'receiving', checkpoint },
  })
  assertBrowserDeliverySource(previous, next)
  return next
}

export function assertBrowserDeliverySource(previous: BrowserDeliveryRecordV1, next: BrowserDeliveryRecordV1): void {
  if (previous.source.fileRevision === next.source.fileRevision) {
    if (previous.source.sourceMetadata !== next.source.sourceMetadata) throw new TypeError('Immutable source evidence changed')
    return
  }
  if (previous.state.kind !== 'receiving' || next.state.kind !== 'receiving' ||
      previous.localMutation !== undefined || next.state.checkpoint?.reusedFrom !== previous.source.fileRevision ||
      compareRevisionMetadata(previous.source, next.source).kind !== 'metadata-agreement') {
    throw new TypeError('Immutable delivery source requires a durable metadata reuse decision')
  }
  const before = previous.state.checkpoint
  const after = next.state.checkpoint
  // A logical record ID is not local object authority; check the independently owned file.
  if (before !== undefined && (before.recordId !== after.recordId || before.ownedObjectId !== after.ownedObjectId ||
      before.checkpointGeneration >= after.checkpointGeneration || before.stateGeneration >= after.stateGeneration ||
      !before.verifiedRanges.every(range => after.verifiedRanges.some(saved => saved.start <= range.start && saved.end >= range.end)))) {
    throw new TypeError('Source admission lost durable ranges or local object ownership')
  }
}
