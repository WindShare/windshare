import { snapshotIdentity } from './canonical'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  receiveOperationRecordPrefix,
  validateManifestPageRecord,
  validatePersistedReceiveRecord,
  validateReceiveOperationHandleRecord,
  validateReceiveOperationLeaseRecord,
  type ManifestPageRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
  type ReceiveRecordKind,
  type WorkspaceActivationCandidateV1,
} from './records'
import type { ReceiveLifecycleState } from './state'
import {
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from './state-codec'

export interface ReceiveOperationTransition {
  readonly operationId: string
  readonly expectedLifecycleGeneration?: bigint
  readonly expectedLeaseId?: string
  readonly records?: readonly PersistedReceiveRecord[]
  readonly manifestPages?: readonly ManifestPageRecord[]
  readonly handles?: readonly ReceiveOperationHandleRecord[]
  readonly deleteRecordIds?: readonly string[]
  readonly deleteManifestPageIds?: readonly string[]
  readonly deleteHandleIds?: readonly string[]
  readonly lifecycle?: ReceiveLifecycleState
  readonly lease?:
    | Readonly<{ kind: 'put'; record: ReceiveOperationLeaseRecord }>
    | Readonly<{ kind: 'delete'; leaseId: string }>
}

export interface PreparedReceiveOperationTransition {
  readonly operationId: string
  readonly expectedLifecycleGeneration?: bigint
  readonly expectedLeaseId?: string
  readonly records: readonly PersistedReceiveRecord[]
  readonly manifestPages: readonly ManifestPageRecord[]
  readonly handles: readonly ReceiveOperationHandleRecord[]
  readonly deleteRecordIds: readonly string[]
  readonly deleteManifestPageIds: readonly string[]
  readonly deleteHandleIds: readonly string[]
  readonly lease?: ReceiveOperationTransition['lease']
}

export interface ReceiveOperationRepository {
  commitTransition(transition: ReceiveOperationTransition): Promise<void>
  readRecord(id: string): Promise<PersistedReceiveRecord | undefined>
  readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined>
  listRecords(operationId: string, kind?: ReceiveRecordKind): Promise<readonly PersistedReceiveRecord[]>
  listManifestPages(operationId: string, kind?: ReceiveRecordKind): Promise<readonly ManifestPageRecord[]>
  readHandle<T = unknown>(id: string): Promise<ReceiveOperationHandleRecord<T> | undefined>
  readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined>
  close(): void
}

/** Terminal cleanup requires an exact, operation-confined handle inventory. */
export interface ReceiveOperationHandleInventoryRepository extends ReceiveOperationRepository {
  listHandles(operationId: string): Promise<readonly ReceiveOperationHandleRecord[]>
}

/** Global enumeration is deliberately limited to journaled activation identities. */
export interface WorkspaceActivationJournalRepository
extends ReceiveOperationHandleInventoryRepository {
  listWorkspaceActivationCandidates(): Promise<readonly WorkspaceActivationCandidateV1[]>
  listInitialWorkspaceActivationOperationIds(): Promise<readonly string[]>
}

export async function prepareReceiveOperationTransition(
  transition: ReceiveOperationTransition,
): Promise<PreparedReceiveOperationTransition> {
  const operationId = snapshotIdentity(transition.operationId, 16, 'operation ID')
  const expectedLifecycleGeneration = snapshotExpectedGeneration(
    transition.expectedLifecycleGeneration,
  )
  const expectedLeaseId = transition.expectedLeaseId === undefined
    ? undefined
    : snapshotIdentity(transition.expectedLeaseId, 16, 'expected lease ID')

  const records = [...(transition.records ?? [])]
  if (transition.lifecycle !== undefined) {
    if (transition.lifecycle.operationId !== operationId) {
      throw new TypeError('lifecycle transition escaped its operation')
    }
    records.push(await storedReceiveLifecycleState(transition.lifecycle))
  }
  assertNoAggregateRangeAuthority(records)
  const validatedRecords = await Promise.all(records.map(validatePersistedReceiveRecord))
  for (const record of validatedRecords) {
    if (record.kind === RECEIVE_RECORD_LIFECYCLE_STATE) {
      decodeStoredReceiveLifecycleState(record)
    }
  }
  const pages = await Promise.all(
    [...(transition.manifestPages ?? [])].map(validateManifestPageRecord),
  )
  const handles = [...(transition.handles ?? [])].map(validateReceiveOperationHandleRecord)
  for (const value of [...validatedRecords, ...pages, ...handles]) {
    if (value.operationId !== operationId) {
      throw new TypeError('atomic transition contains a foreign operation record')
    }
  }

  assertUniqueIds(validatedRecords, 'record')
  assertUniqueIds(pages, 'manifest page')
  assertUniqueIds(handles, 'handle')
  validateLifecycleGeneration(transition.lifecycle, expectedLifecycleGeneration)
  const lease = validateLeaseMutation(transition.lease, operationId, expectedLeaseId)

  return Object.freeze({
    operationId,
    ...(expectedLifecycleGeneration === undefined ? {} : { expectedLifecycleGeneration }),
    ...(expectedLeaseId === undefined ? {} : { expectedLeaseId }),
    records: Object.freeze(validatedRecords),
    manifestPages: Object.freeze(pages),
    handles: Object.freeze(handles),
    deleteRecordIds: snapshotDeleteIds(
      transition.deleteRecordIds,
      'record',
      receiveOperationRecordPrefix(operationId),
    ),
    deleteManifestPageIds: snapshotDeleteIds(
      transition.deleteManifestPageIds,
      'manifest page',
      `windshare/manifest-page/v1/${operationId}/`,
    ),
    deleteHandleIds: snapshotDeleteIds(transition.deleteHandleIds, 'handle'),
    ...(lease === undefined ? {} : { lease }),
  })
}

function validateLifecycleGeneration(
  lifecycle: ReceiveLifecycleState | undefined,
  expected: bigint | undefined,
): void {
  if (lifecycle === undefined) return
  if (expected === undefined) {
    if (lifecycle.generation !== 1n) {
      throw new TypeError('first lifecycle record must start at generation one')
    }
    return
  }
  if (lifecycle.generation !== expected + 1n) {
    throw new TypeError('lifecycle transition did not advance exactly one generation')
  }
}

function validateLeaseMutation(
  mutation: ReceiveOperationTransition['lease'],
  operationId: string,
  expectedLeaseId: string | undefined,
): ReceiveOperationTransition['lease'] {
  if (mutation === undefined) return undefined
  if (mutation.kind === 'put') {
    const record = validateReceiveOperationLeaseRecord(mutation.record)
    if (record.operationId !== operationId) {
      throw new TypeError('lease transition escaped its operation')
    }
    return Object.freeze({ kind: 'put', record })
  }
  const leaseId = snapshotIdentity(mutation.leaseId, 16, 'lease ID')
  if (expectedLeaseId === undefined || leaseId !== expectedLeaseId) {
    throw new TypeError('lease deletion must name the expected current lease')
  }
  return Object.freeze({ kind: 'delete', leaseId })
}

function snapshotExpectedGeneration(value: bigint | undefined): bigint | undefined {
  if (value === undefined) return undefined
  if (typeof value !== 'bigint' || value < 1n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('expected lifecycle generation is invalid')
  }
  return value
}

function snapshotDeleteIds(
  values: readonly string[] | undefined,
  label: string,
  requiredPrefix?: string,
): readonly string[] {
  const ids = [...(values ?? [])]
  if (ids.some((id) =>
    typeof id !== 'string' ||
    id.length === 0 ||
    (requiredPrefix !== undefined && !id.startsWith(requiredPrefix))) ||
      new Set(ids).size !== ids.length) {
    throw new TypeError(`atomic transition contains invalid or repeated ${label} deletion`)
  }
  return Object.freeze(ids)
}

function assertUniqueIds(
  values: readonly { readonly id: string }[],
  label: string,
): void {
  const ids = new Set<string>()
  for (const value of values) {
    if (ids.has(value.id)) throw new TypeError(`atomic transition repeats a ${label} ID`)
    ids.add(value.id)
  }
}

function assertNoAggregateRangeAuthority(records: readonly PersistedReceiveRecord[]): void {
  for (const record of records) {
    const keys = Object.keys(record)
    if (keys.some((key) => /(?:^|_)(?:verified_?)?ranges?$/iu.test(key))) {
      throw new TypeError('aggregate record cannot own verified ranges')
    }
  }
}
