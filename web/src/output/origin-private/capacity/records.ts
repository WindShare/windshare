import type { ObjectCapacityRecord } from '../object-capacity'
import type { StagingBudgetRecord } from '../../staging-budget/contracts'
import { WORKSPACE_CLAIM_STORE, WORKSPACE_OBJECT_STORE, STAGING_FILE_STORE } from './database'
import { OriginCapacityDataError } from './errors'

const MAX_CAPACITY_BYTES = 0xffff_ffff_ffff_ffffn
export const OBJECT_RESERVATION_BOUND = 1_024
const STAGING_PHASES = ['receiving', 'queued', 'exporting', 'export-failed', 'target-saved']

export interface WorkspaceBudgetLeaseRecord {
  readonly id: string
  readonly operationId: string
  readonly token: string
  readonly budgetDigest: string
  readonly peakOwnedBytes: bigint
  readonly expiresAtMilliseconds: number
}

export interface WorkspaceCapacityAccount extends WorkspaceBudgetLeaseRecord {
  readonly occupiedBytes: bigint
  readonly outstandingGrowthBytes: bigint
  readonly metadataHeadroomBytes: bigint
}

export function workspaceCapacityAccount(value: unknown): WorkspaceCapacityAccount {
  const record = capacityRecord(value, WORKSPACE_CLAIM_STORE)
  strings(record, WORKSPACE_CLAIM_STORE, ['operationId', 'budgetDigest'])
  if (record.id !== record.operationId) invalid(record, WORKSPACE_CLAIM_STORE, 'operationId', 'must equal id')
  bytes(record, WORKSPACE_CLAIM_STORE, [
    'peakOwnedBytes', 'occupiedBytes', 'outstandingGrowthBytes', 'metadataHeadroomBytes',
  ])
  if (typeof record.token !== 'string') invalid(record, WORKSPACE_CLAIM_STORE, 'token', 'expected string')
  const expires = record.expiresAtMilliseconds
  if (typeof expires !== 'number' || !Number.isSafeInteger(expires) || expires < 0) {
    invalid(record, WORKSPACE_CLAIM_STORE, 'expiresAtMilliseconds', 'expected nonnegative safe integer')
  }
  return record as unknown as WorkspaceCapacityAccount
}

export function workspaceObjectCapacity(value: unknown): ObjectCapacityRecord {
  const record = capacityRecord(value, WORKSPACE_OBJECT_STORE)
  strings(record, WORKSPACE_OBJECT_STORE, ['operationId', 'objectId', 'token'])
  if (record.id !== record.operationId + ':' + record.objectId) {
    invalid(record, WORKSPACE_OBJECT_STORE, 'id', 'must bind operationId and objectId')
  }
  bytes(record, WORKSPACE_OBJECT_STORE, ['occupiedBytes'])
  if (!Array.isArray(record.reservations) || record.reservations.length > OBJECT_RESERVATION_BOUND) {
    invalid(record, WORKSPACE_OBJECT_STORE, 'reservations', 'expected bounded reservation array')
  }
  const identities = new Set<string>()
  for (const [index, value] of (record.reservations as unknown[]).entries()) {
    const reservation = capacityRecord(value, WORKSPACE_OBJECT_STORE, record.id as string, `reservations[${index}]`)
    strings(reservation, WORKSPACE_OBJECT_STORE, ['reservationId'])
    bytes(reservation, WORKSPACE_OBJECT_STORE, ['targetLength', 'metadataHeadroom'])
    const id = reservation.reservationId as string
    if (identities.has(id)) invalid(record, WORKSPACE_OBJECT_STORE, 'reservations', 'duplicate reservationId')
    identities.add(id)
  }
  return record as unknown as ObjectCapacityRecord
}

export function stagingFileCapacity(value: unknown): StagingBudgetRecord {
  const record = capacityRecord(value, STAGING_FILE_STORE)
  strings(record, STAGING_FILE_STORE, ['operationId', 'fileId', 'token'])
  for (const field of ['objectId', 'exportOwnerId']) {
    if (record[field] !== undefined) strings(record, STAGING_FILE_STORE, [field])
  }
  bytes(record, STAGING_FILE_STORE, ['exactSize', 'verifiedStagedBytes', 'headroomBytes'])
  if ((record.verifiedStagedBytes as bigint) > (record.exactSize as bigint)) {
    invalid(record, STAGING_FILE_STORE, 'verifiedStagedBytes', 'exceeds exactSize')
  }
  if (!STAGING_PHASES.some(phase => phase === record.phase)) {
    invalid(record, STAGING_FILE_STORE, 'phase', 'unknown staging phase')
  }
  return record as unknown as StagingBudgetRecord
}

function capacityRecord(value: unknown, store: string, id?: string, field = 'record'): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new OriginCapacityDataError(`store=${store} id=${id ?? '?'} field=${field}: expected record`)
  }
  const record = value as Record<string, unknown>
  // Nested reservations have no primary key, but errors must still identify their owning object.
  if (id !== undefined) return { ...record, id }
  strings(record, store, ['id'])
  return record
}

function strings(record: Record<string, unknown>, store: string, fields: readonly string[]): void {
  for (const field of fields) {
    if (typeof record[field] !== 'string' || record[field].length === 0) {
      invalid(record, store, field, 'expected nonempty string')
    }
  }
}

function bytes(record: Record<string, unknown>, store: string, fields: readonly string[]): void {
  for (const field of fields) {
    const value = record[field]
    if (typeof value !== 'bigint' || value < 0n || value > MAX_CAPACITY_BYTES) {
      invalid(record, store, field, 'expected u64 bigint')
    }
  }
}

function invalid(record: Record<string, unknown>, store: string, field: string, reason: string): never {
  const value = record[field]
  const valueType = value === null ? 'null' : typeof value
  const actual = typeof value === 'number' ? String(value) : valueType
  throw new OriginCapacityDataError(`store=${store} id=${String(record.id)} field=${field}: ${reason}; actual=${actual}`)
}
