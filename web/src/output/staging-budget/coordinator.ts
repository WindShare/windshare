import { assertStagingExportAuthority, type StagingExportAuthority } from './export-authority'
import type { BrowserStagingStorageFacts } from '../planning/staging-storage'
import { requireCapacityLength, type ObjectCapacity } from '../origin-private/object-capacity'
import { stagingAdmissionReason, stagingCapacityTotals } from './admission'
import { DEFAULT_STAGING_BUDGET_POLICY, type StagingBudgetDeferralReason, type StagingBudgetInventory,
  type StagingBudgetPhase, type StagingBudgetPolicy, type StagingBudgetRecord, type StagingBudgetStore,
  type StagingBudgetTraceEvent } from './contracts'

export interface StagingFileBudgetRequest {
  readonly operationId: string
  readonly fileId: string
  readonly exactSize: bigint
}

export type StagingFileBudgetDecision =
  | Readonly<{ kind: 'admitted'; reservation: StagingFileReservation }>
  | Readonly<{ kind: 'deferred'; reason: StagingBudgetDeferralReason }>

export class StagingBudgetCoordinator {
  readonly #store: StagingBudgetStore
  readonly #storage: () => Promise<BrowserStagingStorageFacts>
  readonly #policy: StagingBudgetPolicy
  readonly #token: () => string
  readonly #trace: ((event: StagingBudgetTraceEvent) => void) | undefined

  constructor(input: Readonly<{
    store: StagingBudgetStore
    storage: () => Promise<BrowserStagingStorageFacts>
    policy?: StagingBudgetPolicy
    randomToken?: () => string
    trace?: (event: StagingBudgetTraceEvent) => void
  }>) {
    this.#store = input.store
    this.#storage = input.storage
    this.#policy = Object.freeze({ ...input.policy ?? DEFAULT_STAGING_BUDGET_POLICY })
    validatePolicy(this.#policy)
    this.#token = input.randomToken ?? (() => crypto.randomUUID())
    this.#trace = input.trace
  }

  async tryReserve(input: StagingFileBudgetRequest): Promise<StagingFileBudgetDecision> {
    const record = this.#record(input, 0n, 'receiving')
    const storage = await this.#storage()
    const reason = await this.#store.transact((inventory) => {
      const reason = stagingAdmissionReason({ inventory, candidate: record, policy: this.#policy, storage })
      return reason === null ? { put: record, result: null } : { result: reason }
    })
    this.#emit({ name: reason === null ? 'receive.staging.admitted' : 'receive.staging.deferred',
      operation_id: input.operationId, file_id: input.fileId, exact_size: input.exactSize,
      quota_kind: storage.quota.kind, ...(reason === null ? {} : { reason }) })
    return reason === null
      ? Object.freeze({ kind: 'admitted', reservation: this.#reservation(record) })
      : Object.freeze({ kind: 'deferred', reason })
  }

  /** The complete journal inventory is read under the operation lease before new file admission. */
  async reconcileUnused(input: Readonly<{
    operationId: string
    deliveryFileIds: readonly string[]
    operationLease: Readonly<{ operationId: string; leaseId: string }>
  }>): Promise<number> {
    if (input.operationLease.operationId !== input.operationId || input.operationLease.leaseId.length === 0) {
      throw new TypeError('Unused staging reconciliation requires its operation lease')
    }
    if (!Array.isArray(input.deliveryFileIds) || input.deliveryFileIds.some(id => typeof id !== 'string' || id.length === 0)) {
      throw new TypeError('Unused staging reconciliation requires a complete delivery file inventory')
    }
    const retained = new Set(input.deliveryFileIds)
    const removed = await this.#store.transact(({ records }) => {
      const unused = records.filter(record => record.operationId === input.operationId &&
        !retained.has(record.fileId) && record.phase === 'receiving' &&
        record.objectId === undefined && record.verifiedStagedBytes === 0n)
      return { deleteIds: unused.map(record => record.id), result: unused }
    })
    for (const record of removed) this.#emit({ name: 'receive.staging.released',
      operation_id: record.operationId, file_id: record.fileId })
    return removed.length
  }

  /** The caller owns the recovered operation lease and has ended any old destination writer. */
  async restore(input: StagingFileBudgetRequest & Readonly<{
    verifiedStagedBytes: bigint
    phase: Exclude<StagingBudgetPhase, 'exporting'>
    operationLease: Readonly<{ operationId: string; leaseId: string }>
  }>): Promise<StagingFileReservation> {
    if (input.operationLease.operationId !== input.operationId || input.operationLease.leaseId.length === 0) {
      throw new TypeError('Staging recovery requires its operation lease')
    }
    const record = this.#record(input, input.verifiedStagedBytes, input.phase)
    await this.#store.transact(({ records }) => {
      const existing = records.find((entry) => entry.id === record.id)
      if (existing !== undefined && existing.exactSize !== record.exactSize) {
        throw new DOMException('Retained staging size changed', 'InvalidStateError')
      }
      // Retained bytes remain obligations even when quota or configured limits have shrunk.
      return { put: { ...record,
        headroomBytes: existing !== undefined && existing.headroomBytes > record.headroomBytes
          ? existing.headroomBytes : record.headroomBytes,
        ...(existing?.objectId === undefined ? {} : { objectId: existing.objectId }) }, result: undefined }
    })
    this.#emit({ name: 'receive.staging.restored', operation_id: input.operationId,
      file_id: input.fileId, exact_size: input.exactSize, phase: input.phase })
    return this.#reservation(record)
  }

  async snapshot(operationId?: string) {
    return this.#store.transact(({ records }) => ({ result: stagingCapacityTotals(
      operationId === undefined ? records : records.filter((record) => record.operationId === operationId),
    ) }))
  }

  #record(input: StagingFileBudgetRequest, verifiedStagedBytes: bigint, phase: StagingBudgetPhase): StagingBudgetRecord {
    if (input.operationId.length === 0 || input.fileId.length === 0) throw new TypeError('Staging identity is empty')
    requireCapacityLength(input.exactSize)
    requireCapacityLength(verifiedStagedBytes)
    if (verifiedStagedBytes > input.exactSize || (phase !== 'receiving' && verifiedStagedBytes !== input.exactSize)) {
      throw new RangeError('Staging state disagrees with verified content')
    }
    const token = this.#token()
    if (token.length === 0) throw new TypeError('Staging owner token is empty')
    return Object.freeze({ operationId: input.operationId, fileId: input.fileId, exactSize: input.exactSize,
      id: JSON.stringify([input.operationId, input.fileId]), token,
      headroomBytes: requireCapacityLength(this.#policy.metadataHeadroomBytes + this.#policy.finalizationHeadroomBytes),
      verifiedStagedBytes, phase })
  }

  #reservation(record: StagingBudgetRecord): StagingFileReservation {
    return new StagingFileReservation(this.#store, record, (event) => this.#emit(event))
  }

  #emit(event: StagingBudgetTraceEvent): void {
    try { this.#trace?.(event) } catch { /* Diagnostics cannot change committed capacity ownership. */ }
  }
}

export class StagingFileReservation {
  readonly #store: StagingBudgetStore
  readonly #identity: StagingBudgetRecord
  readonly #trace: (event: StagingBudgetTraceEvent) => void
  readonly objectCapacity: ObjectCapacity
  #exportAuthority: StagingExportAuthority | undefined

  constructor(store: StagingBudgetStore, record: StagingBudgetRecord,
    trace: (event: StagingBudgetTraceEvent) => void) {
    this.#store = store
    this.#identity = record
    this.#trace = trace
    this.objectCapacity = { reserveGrowth: async (input) => {
      if (input.operationId !== record.operationId || input.targetLength > record.exactSize ||
          input.currentLength > record.exactSize || input.metadataHeadroom > record.headroomBytes) {
        throw new RangeError('Staged object growth escaped its full-file reservation')
      }
      requireCapacityLength(input.targetLength)
      requireCapacityLength(input.currentLength)
      requireCapacityLength(input.metadataHeadroom)
      await this.#store.transact((inventory) => {
        const owned = this.#owned(inventory)
        requirePhase(owned, ['receiving'])
        if (input.objectId.length === 0 || (owned.objectId !== undefined && owned.objectId !== input.objectId)) {
          throw new TypeError('Full-file staging reservation belongs to another object')
        }
        return { put: { ...owned, objectId: input.objectId }, result: undefined }
      })
      // The full file remains reserved even after a failed write; only durable checkpoints reduce future growth.
      return Object.freeze({ reservationId: crypto.randomUUID(),
        settle: async (actualLength: bigint) => {
          requireCapacityLength(actualLength)
          if (actualLength > record.exactSize) throw new RangeError('Staged object exceeded its exact size')
        },
        release: async () => undefined })
    } }
  }

  received(verifiedBytes: bigint): Promise<void> {
    return this.#change((record) => {
      requirePhase(record, ['receiving'])
      requireCapacityLength(verifiedBytes)
      if (verifiedBytes < record.verifiedStagedBytes || verifiedBytes > record.exactSize) {
        throw new RangeError('Staging verified progress is not monotonic')
      }
      return { ...record, verifiedStagedBytes: verifiedBytes }
    })
  }

  queueExport(): Promise<void> {
    return this.#change((record) => {
      requirePhase(record, ['receiving', 'queued'])
      if (record.verifiedStagedBytes !== record.exactSize) throw new TypeError('Export requires complete verified staging')
      return { ...record, phase: 'queued' }
    })
  }

  async beginExport(authority: StagingExportAuthority): Promise<boolean> {
    assertStagingExportAuthority(authority, this.#store.coordinationScope)
    const result = await this.#store.transact((inventory) => {
      assertStagingExportAuthority(authority, this.#store.coordinationScope)
      const record = this.#owned(inventory)
      requirePhase(record, ['queued', 'export-failed', 'exporting'])
      const exporting = inventory.records.filter(entry => entry.phase === 'exporting')
      if (exporting.some(entry => entry.exportOwnerId === authority.ownerId)) {
        return { result: { started: false, recovered: [] as StagingBudgetRecord[] } }
      }
      // Acquiring a fresh exclusive callback proves every previous export callback ended.
      // File obligations stay intact; only the vanished exporter claim is retired.
      const recovered = exporting.map(entry => ({ ...entry, phase: 'export-failed' as const }))
      return { puts: [...recovered, { ...record, phase: 'exporting', exportOwnerId: authority.ownerId }],
        result: { started: true, recovered } }
    })
    for (const record of result.recovered) this.#trace({ name: 'receive.staging.export-recovered',
      operation_id: record.operationId, file_id: record.fileId, phase: 'export-failed' })
    if (result.started) {
      this.#exportAuthority = authority
      this.#emit('exporting')
    }
    return result.started
  }

  async exportFailed(): Promise<void> {
    const authority = this.#assertExportAuthority()
    return this.#change((record) => {
      this.#assertCurrentExport(record, authority)
      requirePhase(record, ['exporting', 'export-failed'])
      return { ...record, phase: 'export-failed' }
    })
  }

  /** Call only after the destination proof has committed durably. */
  async targetSaved(): Promise<void> {
    const authority = this.#assertExportAuthority()
    return this.#change((record) => {
      this.#assertCurrentExport(record, authority)
      requirePhase(record, ['exporting', 'target-saved'])
      return { ...record, phase: 'target-saved' }
    })
  }

  /** A target proof alone cannot release occupied OPFS bytes; deletion must have succeeded. */
  async releaseDeleted(): Promise<void> {
    await this.#store.transact((inventory) => {
      const record = this.#owned(inventory)
      requirePhase(record, ['target-saved'])
      return { deleteId: record.id, result: undefined }
    })
    this.#trace({ name: 'receive.staging.released', operation_id: this.#identity.operationId,
      file_id: this.#identity.fileId })
  }

  /** Explicit discard authority has drained readers/writers and confirmed the owned stage is absent. */
  async releaseDiscarded(): Promise<void> {
    await this.#store.transact((inventory) => {
      const record = this.#owned(inventory)
      return { deleteId: record.id, result: undefined }
    })
    this.#trace({ name: 'receive.staging.released', operation_id: this.#identity.operationId,
      file_id: this.#identity.fileId })
  }

  /** Caller proves no staged object was created; this is not cancellation of recoverable content. */
  async cancelUnused(): Promise<void> {
    await this.#store.transact((inventory) => {
      const record = this.#owned(inventory)
      requirePhase(record, ['receiving'])
      if (record.verifiedStagedBytes !== 0n || record.objectId !== undefined) {
        throw new TypeError('Cannot discard a used staging reservation')
      }
      return { deleteId: record.id, result: undefined }
    })
  }

  #assertExportAuthority(): StagingExportAuthority {
    if (this.#exportAuthority === undefined) {
      throw new DOMException('Staging export owner is absent', 'InvalidStateError')
    }
    assertStagingExportAuthority(this.#exportAuthority, this.#store.coordinationScope)
    return this.#exportAuthority
  }

  #assertCurrentExport(record: StagingBudgetRecord, authority: StagingExportAuthority): void {
    assertStagingExportAuthority(authority, this.#store.coordinationScope)
    if (record.exportOwnerId !== authority.ownerId) {
      throw new DOMException('Staging exporter ownership changed', 'InvalidStateError')
    }
  }

  #owned(inventory: StagingBudgetInventory): StagingBudgetRecord {
    const record = inventory.records.find((entry) => entry.id === this.#identity.id)
    if (record?.token !== this.#identity.token) {
      throw new DOMException('Staging reservation ownership changed', 'InvalidStateError')
    }
    return record
  }

  async #change(update: (record: StagingBudgetRecord) => StagingBudgetRecord): Promise<void> {
    const phase = await this.#store.transact((inventory) => {
      const next = update(this.#owned(inventory))
      return { put: next, result: next.phase }
    })
    this.#emit(phase)
  }

  #emit(phase: StagingBudgetPhase): void {
    this.#trace({ name: 'receive.staging.transition', operation_id: this.#identity.operationId,
      file_id: this.#identity.fileId, phase })
  }
}

function requirePhase(record: StagingBudgetRecord, allowed: readonly StagingBudgetPhase[]): void {
  if (!allowed.includes(record.phase)) throw new DOMException('Staging reservation phase changed', 'InvalidStateError')
}

function validatePolicy(policy: StagingBudgetPolicy): void {
  for (const value of [policy.maximumTaskFiles, policy.maximumSiteFiles]) {
    if (!Number.isSafeInteger(value) || value <= 0) throw new RangeError('Staging file limit must be positive')
  }
  for (const value of [policy.maximumTaskPhysicalBytes, policy.maximumSitePhysicalBytes,
    policy.metadataHeadroomBytes, policy.finalizationHeadroomBytes, policy.minimumQuotaReserveBytes]) {
    requireCapacityLength(value)
  }
}
