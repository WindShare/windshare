import { compareRevisionMetadata } from '../../../content/revision-comparison'
import type { BrowserDeliveryRecordV1 } from '../model'

export function assertBrowserDeliverySource(previous: BrowserDeliveryRecordV1, next: BrowserDeliveryRecordV1): void {
  if (previous.source.fileRevision === next.source.fileRevision) {
    if (previous.source.sourceMetadata !== next.source.sourceMetadata) throw new TypeError('Immutable source evidence changed')
    return
  }
  if ((previous.state.kind !== 'receiving' && previous.state.kind !== 'restart-authorized') ||
      next.state.kind !== 'receiving' || previous.localMutation !== undefined ||
      next.state.checkpoint?.reusedFrom !== previous.source.fileRevision ||
      compareRevisionMetadata(previous.source, next.source).kind !== 'metadata-agreement') {
    throw new TypeError('Immutable delivery source requires a durable metadata reuse decision')
  }
  const before = previous.state.checkpoint
  const after = next.state.checkpoint
  // Applicability cannot change local ownership. The lifecycle separately requires
  // retained coverage for receiving, or an authorized empty cut for a restart.
  if (before !== undefined && (before.recordId !== after.recordId || before.ownedObjectId !== after.ownedObjectId ||
      before.checkpointGeneration >= after.checkpointGeneration || before.stateGeneration >= after.stateGeneration)) {
    throw new TypeError('Source admission changed local object ownership or did not advance its checkpoint')
  }
}
