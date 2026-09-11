import type { FileCheckpointV2 } from '../persistence/checkpoint'
import type { FileCheckpointJournal } from '../persistence/journal'

/** A file ID has one authenticated placement; conflicts never select the newest arbitrary record. */
export async function readBrowserDeliveryCheckpoint(
  journal: Pick<FileCheckpointJournal, 'scanCommitted'>,
  fileId: string,
): Promise<FileCheckpointV2 | undefined> {
  const page = await journal.scanCommitted({ direction: 'descending', fileId, limit: 2 })
  if (page.records.length > 1 || page.nextCursor !== undefined) {
    throw new DOMException('Browser delivery file has conflicting checkpoint lineages', 'InvalidStateError')
  }
  return page.records[0]
}
