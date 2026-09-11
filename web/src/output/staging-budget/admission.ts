import { requireCapacityLength } from '../origin-private/object-capacity'
import type { BrowserStagingStorageFacts } from '../planning/staging-storage'
import type { StagingBudgetDeferralReason, StagingBudgetInventory, StagingBudgetPolicy,
  StagingBudgetRecord, StagingCapacityTotals } from './contracts'

export function stagingCapacityTotals(records: readonly StagingBudgetRecord[]): StagingCapacityTotals {
  let verifiedStagedBytes = 0n
  let outstandingBytes = 0n
  let reservedStagingBytes = 0n
  let oneExportBytes = 0n
  for (const record of records) {
    requireCapacityLength(record.exactSize)
    requireCapacityLength(record.verifiedStagedBytes)
    requireCapacityLength(record.headroomBytes)
    if (record.verifiedStagedBytes > record.exactSize) throw new TypeError('Staging capacity progress exceeds its file')
    verifiedStagedBytes += record.verifiedStagedBytes
    outstandingBytes += record.exactSize - record.verifiedStagedBytes + record.headroomBytes
    reservedStagingBytes += record.exactSize + record.headroomBytes
    if (record.phase !== 'target-saved' && record.exactSize > oneExportBytes) oneExportBytes = record.exactSize
  }
  return Object.freeze({ verifiedStagedBytes: requireCapacityLength(verifiedStagedBytes),
    outstandingBytes: requireCapacityLength(outstandingBytes),
    reservedStagingBytes: requireCapacityLength(reservedStagingBytes),
    oneExportBytes, physicalDemandBytes: requireCapacityLength(reservedStagingBytes + oneExportBytes) })
}

export function stagingAdmissionReason(input: Readonly<{
  inventory: StagingBudgetInventory
  candidate: StagingBudgetRecord
  policy: StagingBudgetPolicy
  storage: BrowserStagingStorageFacts
}>): StagingBudgetDeferralReason | null {
  const { records, workspace } = input.inventory
  const { candidate, policy, storage } = input
  if (records.some((record) => record.id === candidate.id)) return 'retained-reservation'
  if (storage.opfs === 'unavailable') return 'opfs-unavailable'
  // A completed file is useful work that frees space. Never build an entire directory behind it.
  if (storage.pressure === 'drain-first' || records.some((record) =>
    record.phase === 'queued' || record.phase === 'exporting' || record.phase === 'target-saved')) return 'drain-first'
  // A dormant failed export still consumes its full reservation, but cannot drain by itself.
  // Let unrelated tasks use remaining capacity instead of turning retained history into a site-wide stop.
  const task = records.filter((record) => record.operationId === candidate.operationId)
  if (task.length >= policy.maximumTaskFiles) return 'task-file-limit'
  if (records.length >= policy.maximumSiteFiles) return 'site-file-limit'
  if (stagingCapacityTotals([...task, candidate]).physicalDemandBytes > policy.maximumTaskPhysicalBytes) {
    return 'task-physical-limit'
  }
  const site = stagingCapacityTotals([...records, candidate])
  if (site.physicalDemandBytes > policy.maximumSitePhysicalBytes) return 'site-physical-limit'
  if (storage.quota.kind === 'estimated') {
    const owned = site.verifiedStagedBytes + workspace.occupiedBytes
    const usage = storage.quota.usageBytes > owned ? storage.quota.usageBytes : owned
    // Destination demand is deliberately absent: browser quota describes origin storage only.
    if (usage + site.outstandingBytes + workspace.outstandingBytes + policy.minimumQuotaReserveBytes >
        storage.quota.quotaBytes) return 'quota-insufficient'
  }
  return null
}
