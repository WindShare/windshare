import {
  decodeReceiveIntent,
  validateReceiveIntent,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  assertCanonicalRecordDomain,
  canonicalDigest,
  canonicalFrame,
  canonicalText,
  equalCanonicalBytes,
  canonicalIdentity,
  canonicalRecord,
  concatCanonicalBytes,
  canonicalU8,
  canonicalU64,
  snapshotCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from './canonical'
import { CanonicalRecordReader } from './canonical-reader'

export const RECEIVE_OPERATION_SCHEMA_VERSION = 1 as const
export const MANIFEST_PAGE_ENTRY_LIMIT = 128

export const RECEIVE_RECORD_OPERATION = 1 as const
export const RECEIVE_RECORD_RESERVATION = 2 as const
export const RECEIVE_RECORD_WORKSPACE_BINDING = 3 as const
export const RECEIVE_RECORD_PREPARATION = 4 as const
export const RECEIVE_RECORD_MATERIALIZED_MANIFEST = 5 as const
export const RECEIVE_RECORD_SEALED_MATERIALIZATION = 6 as const
export const RECEIVE_RECORD_PACKAGE = 7 as const
export const RECEIVE_RECORD_PUBLICATION_ATTEMPT = 8 as const
export const RECEIVE_RECORD_LIFECYCLE_STATE = 9 as const
export const RECEIVE_RECORD_RECEIPT = 10 as const
export const RECEIVE_RECORD_CLEANUP = 11 as const
export const RECEIVE_RECORD_WORKSPACE_ACTIVATION = 12 as const

export type ReceiveRecordKind =
  | typeof RECEIVE_RECORD_OPERATION
  | typeof RECEIVE_RECORD_RESERVATION
  | typeof RECEIVE_RECORD_WORKSPACE_BINDING
  | typeof RECEIVE_RECORD_PREPARATION
  | typeof RECEIVE_RECORD_MATERIALIZED_MANIFEST
  | typeof RECEIVE_RECORD_SEALED_MATERIALIZATION
  | typeof RECEIVE_RECORD_PACKAGE
  | typeof RECEIVE_RECORD_PUBLICATION_ATTEMPT
  | typeof RECEIVE_RECORD_LIFECYCLE_STATE
  | typeof RECEIVE_RECORD_RECEIPT
  | typeof RECEIVE_RECORD_CLEANUP
  | typeof RECEIVE_RECORD_WORKSPACE_ACTIVATION

export type OperationReopenKey =
  | Readonly<{ kind: 'none' }>
  | Readonly<{ kind: 'cli-compatible'; compatibleOperationKey: string }>

export interface ReceiveOperationV1 {
  readonly schemaVersion: typeof RECEIVE_OPERATION_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntent: ReceiveIntent
  readonly receiveIntentDigest: string
  readonly planBindingDigest: string
  readonly reopenKey: OperationReopenKey
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface PersistedReceiveRecord {
  readonly id: string
  readonly schemaVersion: typeof RECEIVE_OPERATION_SCHEMA_VERSION
  readonly operationId: string
  readonly kind: ReceiveRecordKind
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
  readonly reopenKey?: string
  readonly state?: number
  readonly expiresAt?: number
  readonly lifecycleGeneration?: string
}

export interface ManifestPageRecord {
  readonly id: string
  readonly schemaVersion: typeof RECEIVE_OPERATION_SCHEMA_VERSION
  readonly operationId: string
  readonly kind: ReceiveRecordKind
  readonly ownerDigest: string
  readonly pageIndex: number
  readonly totalPageCount: number
  readonly entryCount: number
  readonly canonicalEntries: readonly CanonicalBytes[]
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface ReceiveOperationHandleRecord<T = unknown> {
  readonly id: string
  readonly schemaVersion: typeof RECEIVE_OPERATION_SCHEMA_VERSION
  readonly operationId: string
  readonly kind: number
  readonly authorityRef: string
  readonly ownedObjectId?: string
  readonly handle: T
}

export interface ReceiveOperationLeaseRecord {
  readonly id: string
  readonly schemaVersion: typeof RECEIVE_OPERATION_SCHEMA_VERSION
  readonly operationId: string
  readonly leaseId: string
  readonly acquiredAt: number
  readonly heartbeatAt: number
}

export interface WorkspaceActivationCandidateV1 {
  readonly schemaVersion: typeof RECEIVE_OPERATION_SCHEMA_VERSION
  readonly operationId: string
  readonly entryIdentity: string
  readonly rootHandleId: string
  readonly rootOwnedObjectId: string
  readonly repositoryAuthority: string
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

const WORKSPACE_ACTIVATION_CANDIDATE_DOMAIN = 'windshare/workspace-activation-candidate/v1'
const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })

export async function createReceiveOperationV1(input: {
  readonly receiveIntent: ReceiveIntent
  readonly reopenKey?: OperationReopenKey
}): Promise<ReceiveOperationV1> {
  const receiveIntent = await validateReceiveIntent(input.receiveIntent)
  const operationId = snapshotIdentity(receiveIntent.operationId, 16, 'operation ID')
  const receiveIntentDigest = snapshotIdentity(
    receiveIntent.digest,
    32,
    'receive intent digest',
  )
  const planBindingDigest = snapshotIdentity(
    receiveIntent.bindingDigest,
    32,
    'plan binding digest',
  )
  const reopenKey = snapshotReopenKey(input.reopenKey ?? { kind: 'none' })
  const canonicalBytes = canonicalRecord('windshare/receive-operation/v1', 1, [
    canonicalFrame(canonicalIdentity(operationId, 16, 'operation ID')),
    canonicalFrame(receiveIntent.canonicalBytes),
    canonicalFrame(canonicalIdentity(receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(planBindingDigest, 32, 'plan binding digest')),
    canonicalFrame(canonicalReopenKey(reopenKey)),
  ])
  return Object.freeze({
    schemaVersion: RECEIVE_OPERATION_SCHEMA_VERSION,
    operationId,
    receiveIntent,
    receiveIntentDigest,
    planBindingDigest,
    reopenKey,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export function storedReceiveOperationRecord(
  operation: ReceiveOperationV1,
): PersistedReceiveRecord {
  const reopenKey = operation.reopenKey.kind === 'cli-compatible'
    ? operation.reopenKey.compatibleOperationKey
    : undefined
  return Object.freeze({
    id: operationRecordId(operation.operationId, RECEIVE_RECORD_OPERATION),
    schemaVersion: RECEIVE_OPERATION_SCHEMA_VERSION,
    operationId: operation.operationId,
    kind: RECEIVE_RECORD_OPERATION,
    canonicalBytes: snapshotCanonicalBytes(operation.canonicalBytes),
    digest: snapshotIdentity(operation.digest, 32, 'operation record digest'),
    ...(reopenKey === undefined ? {} : { reopenKey }),
  })
}

/**
 * The operation envelope belongs to the workspace repository, while ReceiveIntent
 * remains owned by transfer. Reopen therefore delegates the nested bytes to the
 * canonical transfer decoder and then rebuilds the complete operation record.
 */
export async function decodeStoredReceiveOperation(
  recordInput: PersistedReceiveRecord,
): Promise<ReceiveOperationV1> {
  const record = await validatePersistedReceiveRecord(recordInput)
  if (record.kind !== RECEIVE_RECORD_OPERATION) {
    throw new TypeError('receive operation decoder requires an operation record')
  }
  const reader = CanonicalRecordReader.open(
    record.canonicalBytes,
    'windshare/receive-operation/v1',
    RECEIVE_OPERATION_SCHEMA_VERSION,
  )
  const operationId = reader.framedIdentity(16, 'operation ID')
  const receiveIntentBytes = reader.frame('receive intent')
  const receiveIntentDigest = reader.framedIdentity(32, 'receive intent digest')
  const planBindingDigest = reader.framedIdentity(32, 'plan binding digest')
  const reopenKey = decodeCanonicalReopenKey(reader.frame('operation reopen key'))
  reader.finish('receive operation')

  const receiveIntent = await decodeReceiveIntent(receiveIntentBytes)
  if (!equalCanonicalBytes(receiveIntent.canonicalBytes, receiveIntentBytes)) {
    throw new TypeError('decoded receive intent changed its canonical bytes')
  }
  const rebuilt = await createReceiveOperationV1({ receiveIntent, reopenKey })
  const projection = storedReceiveOperationRecord(rebuilt)
  if (rebuilt.operationId !== operationId ||
      rebuilt.receiveIntentDigest !== receiveIntentDigest ||
      rebuilt.planBindingDigest !== planBindingDigest ||
      record.id !== projection.id || record.operationId !== projection.operationId ||
      record.digest !== projection.digest || record.reopenKey !== projection.reopenKey ||
      !equalCanonicalBytes(record.canonicalBytes, projection.canonicalBytes)) {
    throw new TypeError('persisted receive operation changed its canonical authority')
  }
  return rebuilt
}

export async function createPersistedReceiveRecord(input: {
  readonly operationId: string
  readonly kind: Exclude<ReceiveRecordKind, typeof RECEIVE_RECORD_OPERATION>
  readonly canonicalBytes: Uint8Array
  readonly state?: number
  readonly expiresAt?: number
  readonly lifecycleGeneration?: bigint
}): Promise<PersistedReceiveRecord> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const canonicalBytes = snapshotCanonicalBytes(input.canonicalBytes)
  assertCanonicalRecordDomain(
    canonicalBytes,
    receiveRecordDomain(input.kind),
    RECEIVE_OPERATION_SCHEMA_VERSION,
  )
  validateIndexedProjections(
    input.kind,
    input.state,
    input.expiresAt,
    input.lifecycleGeneration,
  )
  const digest = await canonicalDigest(canonicalBytes)
  return Object.freeze({
    id: operationRecordId(operationId, input.kind, digest),
    schemaVersion: RECEIVE_OPERATION_SCHEMA_VERSION,
    operationId,
    kind: input.kind,
    canonicalBytes,
    digest,
    ...(input.state === undefined ? {} : { state: input.state }),
    ...(input.expiresAt === undefined ? {} : { expiresAt: input.expiresAt }),
    ...(input.lifecycleGeneration === undefined
      ? {}
      : { lifecycleGeneration: canonicalU64Text(input.lifecycleGeneration) }),
  })
}

export async function validatePersistedReceiveRecord(
  record: PersistedReceiveRecord,
): Promise<PersistedReceiveRecord> {
  if (record.schemaVersion !== RECEIVE_OPERATION_SCHEMA_VERSION) {
    throw new TypeError('receive record schema version is invalid')
  }
  assertExactPersistedRecordShape(record)
  const operationId = snapshotIdentity(record.operationId, 16, 'operation ID')
  const digest = snapshotIdentity(record.digest, 32, 'receive record digest')
  const canonicalBytes = snapshotCanonicalBytes(record.canonicalBytes)
  assertCanonicalRecordDomain(canonicalBytes, receiveRecordDomain(record.kind), 1)
  if (await canonicalDigest(canonicalBytes) !== digest) {
    throw new TypeError('receive record digest does not match canonical bytes')
  }
  validateIndexedProjections(
    record.kind,
    record.state,
    record.expiresAt,
    record.lifecycleGeneration,
  )
  if (record.kind !== RECEIVE_RECORD_OPERATION && record.reopenKey !== undefined) {
    throw new TypeError('reopen key exists outside a CLI-compatible operation record')
  }
  if (record.reopenKey !== undefined) snapshotIdentity(record.reopenKey, 32, 'reopen key')
  const expectedId = operationRecordId(operationId, record.kind, digest)
  if (record.id !== expectedId) throw new TypeError('receive record ID is not canonical')
  return Object.freeze({ ...record, operationId, digest, canonicalBytes })
}

export async function validateManifestPageRecord(
  record: ManifestPageRecord,
): Promise<ManifestPageRecord> {
  if (record.schemaVersion !== RECEIVE_OPERATION_SCHEMA_VERSION ||
      record.entryCount !== record.canonicalEntries.length) {
    throw new TypeError('manifest page projections are invalid')
  }
  const rebuilt = await createManifestPageRecord({
    operationId: record.operationId,
    ownerKind: record.kind,
    ownerDigest: record.ownerDigest,
    pageIndex: record.pageIndex,
    totalPageCount: record.totalPageCount,
    canonicalEntries: record.canonicalEntries,
  })
  if (record.id !== rebuilt.id || record.digest !== rebuilt.digest ||
      !equalCanonicalBytes(record.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError('manifest page projections disagree with canonical bytes')
  }
  return rebuilt
}

export async function createManifestPageRecord(input: {
  readonly operationId: string
  readonly ownerKind: ReceiveRecordKind
  readonly ownerDigest: string
  readonly pageIndex: number
  readonly totalPageCount: number
  readonly canonicalEntries: readonly Uint8Array[]
}): Promise<ManifestPageRecord> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const ownerDigest = snapshotIdentity(input.ownerDigest, 32, 'manifest owner digest')
  const pageIndex = pageNumber(input.pageIndex, 'manifest page index')
  const totalPageCount = pageNumber(input.totalPageCount, 'manifest page count')
  if (pageIndex >= totalPageCount || input.canonicalEntries.length > MANIFEST_PAGE_ENTRY_LIMIT) {
    throw new TypeError('manifest page bounds are invalid')
  }
  const canonicalEntries = Object.freeze(input.canonicalEntries.map(snapshotCanonicalBytes))
  const canonicalBytes = canonicalRecord('windshare/manifest-page/v1', 1, [
    canonicalFrame(canonicalIdentity(operationId, 16, 'operation ID')),
    canonicalFrame(canonicalU8(input.ownerKind)),
    canonicalFrame(canonicalIdentity(ownerDigest, 32, 'manifest owner digest')),
    canonicalFrame(canonicalU64(BigInt(pageIndex))),
    canonicalFrame(canonicalU64(BigInt(totalPageCount))),
    canonicalU64(BigInt(canonicalEntries.length)),
    ...canonicalEntries.map(canonicalFrame),
  ])
  const digest = await canonicalDigest(canonicalBytes)
  return Object.freeze({
    id: `windshare/manifest-page/v1/${operationId}/${input.ownerKind}/${ownerDigest}/${pageIndex}`,
    schemaVersion: RECEIVE_OPERATION_SCHEMA_VERSION,
    operationId,
    kind: input.ownerKind,
    ownerDigest,
    pageIndex,
    totalPageCount,
    entryCount: canonicalEntries.length,
    canonicalEntries,
    canonicalBytes,
    digest,
  })
}

export async function createWorkspaceActivationCandidate(input: {
  readonly operationId: string
  readonly entryIdentity: string
  readonly rootHandleId: string
  readonly rootOwnedObjectId: string
  readonly repositoryAuthority: string
}): Promise<WorkspaceActivationCandidateV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const entryIdentity = snapshotIdentity(input.entryIdentity, 32, 'workspace entry identity')
  const rootHandleId = canonicalText(input.rootHandleId)
  if (rootHandleId.byteLength === 0) throw new TypeError('workspace root handle ID is empty')
  const rootOwnedObjectId = snapshotIdentity(
    input.rootOwnedObjectId,
    32,
    'workspace owned object ID',
  )
  const repositoryAuthority = snapshotIdentity(
    input.repositoryAuthority,
    32,
    'workspace repository authority',
  )
  const canonicalBytes = canonicalRecord(WORKSPACE_ACTIVATION_CANDIDATE_DOMAIN, 1, [
    canonicalFrame(canonicalIdentity(operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(entryIdentity, 32, 'workspace entry identity')),
    canonicalFrame(rootHandleId),
    canonicalFrame(canonicalIdentity(rootOwnedObjectId, 32, 'workspace owned object ID')),
    canonicalFrame(canonicalIdentity(repositoryAuthority, 32, 'workspace repository authority')),
  ])
  return Object.freeze({
    schemaVersion: RECEIVE_OPERATION_SCHEMA_VERSION,
    operationId,
    entryIdentity,
    rootHandleId: input.rootHandleId,
    rootOwnedObjectId,
    repositoryAuthority,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export function storedWorkspaceActivationCandidate(
  candidate: WorkspaceActivationCandidateV1,
): Promise<PersistedReceiveRecord> {
  return createPersistedReceiveRecord({
    operationId: candidate.operationId,
    kind: RECEIVE_RECORD_WORKSPACE_ACTIVATION,
    canonicalBytes: candidate.canonicalBytes,
  })
}

export async function decodeStoredWorkspaceActivationCandidate(
  recordInput: PersistedReceiveRecord,
): Promise<WorkspaceActivationCandidateV1> {
  const record = await validatePersistedReceiveRecord(recordInput)
  if (record.kind !== RECEIVE_RECORD_WORKSPACE_ACTIVATION) {
    throw new TypeError('workspace activation decoder requires an activation candidate record')
  }
  const reader = CanonicalRecordReader.open(
    record.canonicalBytes,
    WORKSPACE_ACTIVATION_CANDIDATE_DOMAIN,
    RECEIVE_OPERATION_SCHEMA_VERSION,
  )
  const operationId = reader.framedIdentity(16, 'operation ID')
  const entryIdentity = reader.framedIdentity(32, 'workspace entry identity')
  const rootHandleId = decodeCanonicalText(reader.frame('workspace root handle ID'))
  const rootOwnedObjectId = reader.framedIdentity(32, 'workspace owned object ID')
  const repositoryAuthority = reader.framedIdentity(32, 'workspace repository authority')
  reader.finish('workspace activation candidate')
  const rebuilt = await createWorkspaceActivationCandidate({
    operationId,
    entryIdentity,
    rootHandleId,
    rootOwnedObjectId,
    repositoryAuthority,
  })
  const projection = await storedWorkspaceActivationCandidate(rebuilt)
  if (record.id !== projection.id || record.digest !== projection.digest ||
      !equalCanonicalBytes(record.canonicalBytes, projection.canonicalBytes)) {
    throw new TypeError('workspace activation candidate changed its canonical authority')
  }
  return rebuilt
}

export function validateReceiveOperationHandleRecord<T>(
  record: ReceiveOperationHandleRecord<T>,
): ReceiveOperationHandleRecord<T> {
  if (record.schemaVersion !== RECEIVE_OPERATION_SCHEMA_VERSION) {
    throw new TypeError('operation handle schema version is invalid')
  }
  return receiveOperationHandleRecord(record)
}

export function receiveOperationHandleRecord<T>(input: {
  readonly id: string
  readonly operationId: string
  readonly kind: number
  readonly authorityRef: string
  readonly ownedObjectId?: string
  readonly handle: T
}): ReceiveOperationHandleRecord<T> {
  if (!Number.isInteger(input.kind) || input.kind < 1 || input.kind > 0xff ||
      typeof input.id !== 'string' || input.id.length === 0) {
    throw new TypeError('operation handle identity is invalid')
  }
  return Object.freeze({
    id: input.id,
    schemaVersion: RECEIVE_OPERATION_SCHEMA_VERSION,
    operationId: snapshotIdentity(input.operationId, 16, 'operation ID'),
    kind: input.kind,
    authorityRef: snapshotIdentity(input.authorityRef, 32, 'authority reference'),
    ...(input.ownedObjectId === undefined
      ? {}
      : { ownedObjectId: snapshotIdentity(input.ownedObjectId, 32, 'owned object ID') }),
    handle: input.handle,
  })
}

export function validateReceiveOperationLeaseRecord(
  record: ReceiveOperationLeaseRecord,
): ReceiveOperationLeaseRecord {
  if (record.schemaVersion !== RECEIVE_OPERATION_SCHEMA_VERSION) {
    throw new TypeError('operation lease schema version is invalid')
  }
  const rebuilt = receiveOperationLeaseRecord(record)
  if (record.id !== rebuilt.id) throw new TypeError('operation lease ID is not canonical')
  return rebuilt
}

export function receiveOperationLeaseRecord(input: {
  readonly operationId: string
  readonly leaseId: string
  readonly acquiredAt: number
  readonly heartbeatAt?: number
}): ReceiveOperationLeaseRecord {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const acquiredAt = unixMilliseconds(input.acquiredAt, 'lease acquisition time')
  const heartbeatAt = unixMilliseconds(input.heartbeatAt ?? acquiredAt, 'lease heartbeat time')
  if (heartbeatAt < acquiredAt) throw new TypeError('lease heartbeat predates acquisition')
  return Object.freeze({
    id: `windshare/receive-operation/v1/${operationId}/lease`,
    schemaVersion: RECEIVE_OPERATION_SCHEMA_VERSION,
    operationId,
    leaseId: snapshotIdentity(input.leaseId, 16, 'lease ID'),
    acquiredAt,
    heartbeatAt,
  })
}

export function operationRecordId(
  operationId: string,
  kind: ReceiveRecordKind,
  digest?: string,
): string {
  const operation = snapshotIdentity(operationId, 16, 'operation ID')
  if (kind === RECEIVE_RECORD_OPERATION || kind === RECEIVE_RECORD_LIFECYCLE_STATE) {
    return `windshare/receive-operation/v1/${operation}/${kind}`
  }
  if (digest === undefined) throw new TypeError('immutable receive record requires its digest')
  return `windshare/receive-operation/v1/${operation}/${kind}/${snapshotIdentity(digest, 32, 'record digest')}`
}

export function receiveRecordDomain(kind: ReceiveRecordKind): string {
  switch (kind) {
    case RECEIVE_RECORD_OPERATION: return 'windshare/receive-operation/v1'
    case RECEIVE_RECORD_RESERVATION: return 'windshare/destination-reservation/v1'
    case RECEIVE_RECORD_WORKSPACE_BINDING: return 'windshare/workspace-binding/v1'
    case RECEIVE_RECORD_PREPARATION: return 'windshare/preparation-manifest/v1'
    case RECEIVE_RECORD_MATERIALIZED_MANIFEST: return 'windshare/materialized-manifest/v1'
    case RECEIVE_RECORD_SEALED_MATERIALIZATION: return 'windshare/sealed-materialization/v1'
    case RECEIVE_RECORD_PACKAGE: return 'windshare/packaged-artifact/v1'
    case RECEIVE_RECORD_PUBLICATION_ATTEMPT: return 'windshare/publication-attempt/v1'
    case RECEIVE_RECORD_LIFECYCLE_STATE: return 'windshare/receive-lifecycle-state/v1'
    case RECEIVE_RECORD_RECEIPT:
    case RECEIVE_RECORD_CLEANUP:
      return 'windshare/receive-receipt/v1'
    case RECEIVE_RECORD_WORKSPACE_ACTIVATION:
      return WORKSPACE_ACTIVATION_CANDIDATE_DOMAIN
  }
}

function decodeCanonicalText(bytes: Uint8Array): string {
  const value = TEXT_DECODER.decode(bytes)
  if (!equalCanonicalBytes(canonicalText(value), bytes)) {
    throw new TypeError('canonical text encoding is invalid')
  }
  return value
}

function snapshotReopenKey(input: OperationReopenKey): OperationReopenKey {
  if (input.kind === 'none') return Object.freeze({ kind: 'none' })
  if (input.kind !== 'cli-compatible') throw new TypeError('operation reopen key kind is invalid')
  return Object.freeze({
    kind: 'cli-compatible',
    compatibleOperationKey: snapshotIdentity(
      input.compatibleOperationKey,
      32,
      'compatible operation key',
    ),
  })
}

function canonicalReopenKey(key: OperationReopenKey): CanonicalBytes {
  return key.kind === 'none'
    ? canonicalU8(1)
    : concatCanonicalBytes([
        canonicalU8(2),
        canonicalFrame(canonicalIdentity(
          key.compatibleOperationKey,
          32,
          'compatible operation key',
        )),
      ])
}

function decodeCanonicalReopenKey(bytes: Uint8Array): OperationReopenKey {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('operation reopen key discriminant')
  if (discriminant === 1) {
    reader.finish('operation reopen key')
    return Object.freeze({ kind: 'none' })
  }
  if (discriminant !== 2) throw new TypeError('operation reopen key discriminant is invalid')
  const compatibleOperationKey = reader.framedIdentity(32, 'compatible operation key')
  reader.finish('operation reopen key')
  return Object.freeze({ kind: 'cli-compatible', compatibleOperationKey })
}

function validateIndexedProjections(
  kind: ReceiveRecordKind,
  state: number | undefined,
  expiresAt: number | undefined,
  lifecycleGeneration: string | bigint | undefined,
): void {
  if (kind !== RECEIVE_RECORD_LIFECYCLE_STATE &&
      (state !== undefined || expiresAt !== undefined || lifecycleGeneration !== undefined)) {
    throw new TypeError('state and expiry projections exist only on lifecycle records')
  }
  if (state !== undefined && (!Number.isInteger(state) || state < 1 || state > 20)) {
    throw new TypeError('lifecycle state projection is invalid')
  }
  if (expiresAt !== undefined) unixMilliseconds(expiresAt, 'lifecycle expiry')
  if (kind === RECEIVE_RECORD_LIFECYCLE_STATE) {
    if (state === undefined) {
      throw new TypeError('lifecycle state projection is required')
    }
    if (lifecycleGeneration === undefined) {
      throw new TypeError('lifecycle generation projection is required')
    }
    if (typeof lifecycleGeneration === 'bigint') canonicalU64Text(lifecycleGeneration)
    else if (!/^[1-9]\d*$/u.test(lifecycleGeneration) ||
        BigInt(lifecycleGeneration) > 0xffff_ffff_ffff_ffffn) {
      throw new TypeError('lifecycle generation projection is invalid')
    }
  }
}

function assertExactPersistedRecordShape(record: PersistedReceiveRecord): void {
  if (typeof record !== 'object' || record === null || Array.isArray(record)) {
    throw new TypeError('receive record row is invalid')
  }
  const expected = [
    'id',
    'schemaVersion',
    'operationId',
    'kind',
    'canonicalBytes',
    'digest',
  ]
  if (record.kind === RECEIVE_RECORD_OPERATION && record.reopenKey !== undefined) {
    expected.push('reopenKey')
  }
  if (record.kind === RECEIVE_RECORD_LIFECYCLE_STATE) {
    expected.push('state', 'lifecycleGeneration')
    if (record.expiresAt !== undefined) expected.push('expiresAt')
  }
  const actual = Object.keys(record).sort()
  expected.sort()
  if (actual.length !== expected.length ||
      actual.some((key, index) => key !== expected[index])) {
    throw new TypeError('receive record row contains non-contract fields')
  }
}

function canonicalU64Text(value: bigint): string {
  if (typeof value !== 'bigint' || value < 1n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('lifecycle generation projection is invalid')
  }
  return value.toString(10)
}

function pageNumber(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value < 0) throw new TypeError(`${label} is invalid`)
  return value
}

function unixMilliseconds(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value < 0) throw new TypeError(`${label} is invalid`)
  return value
}
