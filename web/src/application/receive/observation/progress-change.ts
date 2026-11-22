import type { BrowserDeliverySummary } from '../../../output/model/browser-delivery'
import type { BrowserFolderProgressSnapshot } from '../contracts'
import type { ProgressChange } from '../../../transfer/progress/observation'

const DELIVERY_ACTION_COUNTS = [
  'receivingFiles', 'incompleteStagedFiles', 'stagedCompleteFiles', 'copyingFiles',
  'cleanupPendingFiles', 'targetSavedFiles', 'discardedFiles', 'reservedStagingBytes',
] as const satisfies readonly (keyof BrowserDeliverySummary)[]

export function folderProgressChange(previous: BrowserFolderProgressSnapshot | null | undefined, next: BrowserFolderProgressSnapshot): ProgressChange {
  if (previous == null) return 'transition'
  // Presence, rather than every count, determines which local actions are legal.
  return DELIVERY_ACTION_COUNTS.some(key => (previous.summary[key] > 0) !== (next.summary[key] > 0))
    ? 'transition' : 'counters'
}
