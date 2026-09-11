import type { BrowserStagingStorageFacts } from '../planning/staging-storage'

export type StagingBudgetPhase = 'receiving' | 'queued' | 'exporting' | 'export-failed' | 'target-saved'
export interface StagingBudgetRecord {
  readonly id: string
  readonly operationId: string
  readonly fileId: string
  readonly token: string
  readonly objectId?: string
  readonly exactSize: bigint
  readonly verifiedStagedBytes: bigint
  readonly headroomBytes: bigint
  readonly phase: StagingBudgetPhase
  readonly exportOwnerId?: string
}

export interface StagingBudgetPolicy {
  readonly maximumTaskFiles: number
  readonly maximumSiteFiles: number
  readonly maximumTaskPhysicalBytes: bigint
  readonly maximumSitePhysicalBytes: bigint
  readonly metadataHeadroomBytes: bigint
  readonly finalizationHeadroomBytes: bigint
  readonly minimumQuotaReserveBytes: bigint
}

export const DEFAULT_STAGING_BUDGET_POLICY: StagingBudgetPolicy = Object.freeze({
  maximumTaskFiles: 2,
  maximumSiteFiles: 4,
  maximumTaskPhysicalBytes: 512n * 1024n ** 3n,
  maximumSitePhysicalBytes: 1024n * 1024n ** 3n,
  metadataHeadroomBytes: 1024n ** 2n,
  finalizationHeadroomBytes: 4n * 1024n ** 2n,
  minimumQuotaReserveBytes: 512n * 1024n ** 2n,
})

export interface WorkspaceCapacityInventory {
  readonly occupiedBytes: bigint
  readonly outstandingBytes: bigint
}

export interface StagingBudgetInventory {
  readonly records: readonly StagingBudgetRecord[]
  readonly workspace: WorkspaceCapacityInventory
}

export interface StagingBudgetMutation<T> {
  readonly result: T
  readonly put?: StagingBudgetRecord
  readonly deleteId?: string
  readonly puts?: readonly StagingBudgetRecord[]
  readonly deleteIds?: readonly string[]
}

/** Implementations serialize this transaction with existing workspace-growth admissions. */
export interface StagingBudgetStore {
  readonly coordinationScope: 'origin' | 'context'
  transact<T>(update: (inventory: StagingBudgetInventory) => StagingBudgetMutation<T>): Promise<T>
}

export type StagingBudgetDeferralReason =
  | 'opfs-unavailable' | 'drain-first' | 'task-file-limit' | 'site-file-limit'
  | 'task-physical-limit' | 'site-physical-limit' | 'quota-insufficient' | 'retained-reservation'

export interface StagingCapacityTotals {
  readonly verifiedStagedBytes: bigint
  readonly outstandingBytes: bigint
  readonly reservedStagingBytes: bigint
  readonly oneExportBytes: bigint
  readonly physicalDemandBytes: bigint
}

export interface StagingBudgetTraceEvent {
  readonly name: 'receive.staging.admitted' | 'receive.staging.deferred' | 'receive.staging.transition' |
    'receive.staging.released' | 'receive.staging.restored' | 'receive.staging.export-recovered'
  readonly operation_id: string
  readonly file_id: string
  readonly exact_size?: bigint
  readonly phase?: StagingBudgetPhase
  readonly reason?: StagingBudgetDeferralReason
  readonly quota_kind?: BrowserStagingStorageFacts['quota']['kind']
}
