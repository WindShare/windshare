export type BrowserRecoveryPreference = 'automatic' | 'direct'

export interface BrowserDeliverySummary {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly receivingFiles: number
  readonly incompleteStagedFiles: number
  readonly stagedCompleteFiles: number
  readonly copyingFiles: number
  readonly cleanupPendingFiles: number
  readonly targetSavedFiles: number
  readonly discardedFiles: number
  readonly recoverableBytes: bigint
  readonly stagedBytes: bigint
  readonly reservedStagingBytes: bigint
  readonly targetSavedBytes: bigint
}

export function hasBrowserDeliveryStaging(summary: BrowserDeliverySummary): boolean {
  // Empty owned files and reservations still require disposal authority.
  return summary.incompleteStagedFiles + summary.stagedCompleteFiles + summary.copyingFiles + summary.cleanupPendingFiles > 0 ||
    summary.reservedStagingBytes > 0n
}
