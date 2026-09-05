import { catalogNameCollisionKey } from '../../catalog/path-policy'
import {
  validateReceiveOperationHandleRecord,
  type ReceiveOperationHandleRecord,
} from '../workspace/records'
import type { PreparedReceiveOperationTransition } from '../workspace/repository'
import {
  COMPATIBLE_NAME_LEDGER_FORMAT_VERSION,
  MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS,
  MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS,
  compatibleNameMappingId,
  compatibleNameMappingV1,
  compatibleNameOperationBootstrapV1,
  compatibleNameOperationHeaderV1,
  compatibleMappingPhysicalParent,
  compatiblePairPhysicalParent,
  compatibleNameRepairSummary,
  type CompatibleNameMappingV1,
  type CompatibleNameOperationBootstrapV1,
  type CompatibleNameOperationHeaderV1,
  type CompatibleNameOperationSnapshotV1,
  type CompatibleNamePairKind,
  type CompatibleNameRepairSummary,
} from '../file-system-access/compatible-name/model'
import {
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
  INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
  requestResult,
} from './indexeddb-database'

export interface CompatibleNameOperationRowV1 {
  readonly header: CompatibleNameOperationHeaderV1
  readonly nextCommitOrdinal: number
}

type StoredCompatibleNameOperationRowV1 = CompatibleNameOperationHeaderV1 & Readonly<{
  nextCommitOrdinal: number
}>

const COMPATIBLE_NAME_LEDGER_CAPACITY_ERROR =
  'Compatible-name operation exceeds its bounded mapping capacity'

export function assertCompatibleNameBootstrapTransition(
  transition: PreparedReceiveOperationTransition,
  bootstrapInput: CompatibleNameOperationBootstrapV1,
): CompatibleNameOperationBootstrapV1 {
  const bootstrap = compatibleNameOperationBootstrapV1(bootstrapInput)
  if (transition.operationId !== bootstrap.header.operationId) {
    throw new TypeError('compatible-name bootstrap escaped its receive operation')
  }
  const pairHandleIds = new Set([
    bootstrap.header.pair.script.handleId,
    bootstrap.header.pair.sidecar.handleId,
  ])
  if (transition.handles.some(handle => pairHandleIds.has(handle.id))) {
    throw new TypeError('repair pair ownership cannot precede native pair creation')
  }
  return bootstrap
}

export async function assertCompatibleNameBootstrapTransaction(
  transaction: IDBTransaction,
  bootstrap: CompatibleNameOperationBootstrapV1,
): Promise<boolean> {
  const operations = transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
  const existingHeaderValue = await requestResult<unknown>(
    operations.get(bootstrap.header.operationId),
  )
  const mappingValues = await operationMappingValues(transaction, bootstrap.header.operationId)
  if (existingHeaderValue === undefined) {
    if (mappingValues.length !== 0) {
      abortIntegrity(transaction, 'compatible-name mappings exist without their operation header')
    }
    return true
  }
  const existing = readOperationRow(existingHeaderValue)
  if (existing.nextCommitOrdinal !== 1 ||
      !sameValue(existing.header, bootstrap.header) || mappingValues.length !== 1 ||
      !sameValue(readMapping(mappingValues[0]), bootstrap.initialMapping)) {
    abortIntegrity(transaction, 'compatible-name bootstrap is immutable')
  }
  return false
}

export function applyCompatibleNameBootstrapTransaction(
  transaction: IDBTransaction,
  bootstrap: CompatibleNameOperationBootstrapV1,
): void {
  transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
    .add(storedOperationRow(bootstrap.header, 1))
  transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE).add(bootstrap.initialMapping)
}

export function operationSnapshot(
  operation: CompatibleNameOperationRowV1,
  values: readonly unknown[],
): CompatibleNameOperationSnapshotV1 {
  const mappings = values.map(readMapping).sort((left, right) => compareText(left.id, right.id))
  const physicalClaims = new Set<string>()
  const pairClaims = pairPhysicalClaimKeys(operation.header)
  const committedOrdinals: number[] = []
  for (const mapping of mappings) {
    const physicalClaim = mappingPhysicalClaimKey(operation.header, mapping)
    if (mapping.operationId !== operation.header.operationId ||
        physicalClaims.has(physicalClaim) || pairClaims.has(physicalClaim)) {
      throw new TypeError('compatible-name operation snapshot contains conflicting claims')
    }
    physicalClaims.add(physicalClaim)
    if (mapping.commitOrdinal !== undefined) committedOrdinals.push(mapping.commitOrdinal)
  }
  committedOrdinals.sort((left, right) => left - right)
  if (committedOrdinals.length !== operation.nextCommitOrdinal - 1 ||
      committedOrdinals.some((ordinal, index) => ordinal !== index + 1)) {
    throw new TypeError('compatible-name operation snapshot has a non-contiguous commit prefix')
  }
  return Object.freeze({
    header: operation.header,
    mappings: Object.freeze(mappings),
  })
}

export async function requireOperationRow(
  transaction: IDBTransaction,
  operationId: string,
): Promise<CompatibleNameOperationRowV1> {
  const value = await requestResult<unknown>(
    transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE).get(operationId),
  )
  if (value === undefined) abortIntegrity(transaction, 'compatible-name operation header is missing')
  return readOperationRow(value)
}

export async function operationMappingValues(
  transaction: IDBTransaction,
  operationId: string,
): Promise<readonly unknown[]> {
  const values = await requestResult<unknown[]>(
    transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE)
      .index(INDEXEDDB_BY_OPERATION_INDEX)
      .getAll(IDBKeyRange.only(operationId), MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS + 1),
  )
  if (values.length > MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS) abortCapacity(transaction)
  return values
}

export function readOperationRow(value: unknown): CompatibleNameOperationRowV1 {
  const row = value as StoredCompatibleNameOperationRowV1
  if (row?.formatVersion !== COMPATIBLE_NAME_LEDGER_FORMAT_VERSION) {
    throw new TypeError('compatible-name operation row version is invalid')
  }
  const header = compatibleNameOperationHeaderV1(row)
  const nextCommitOrdinal = boundedOrdinal(
    row?.nextCommitOrdinal,
    false,
    'next compatible-name commit ordinal',
  )
  const rebuilt = storedOperationRow(header, nextCommitOrdinal)
  if (!sameValue(value, rebuilt)) throw new TypeError('compatible-name operation row is invalid')
  return operationRow(header, nextCommitOrdinal)
}

export function readMapping(value: unknown): CompatibleNameMappingV1 {
  const row = value as CompatibleNameMappingV1
  if (row?.formatVersion !== COMPATIBLE_NAME_LEDGER_FORMAT_VERSION) {
    throw new TypeError('compatible-name mapping row version is invalid')
  }
  const mapping = compatibleNameMappingV1(row)
  if (row.id !== mapping.id ||
      !sameValue(value, mapping)) {
    throw new TypeError('compatible-name mapping row is invalid')
  }
  return mapping
}

export function operationRow(
  header: CompatibleNameOperationHeaderV1,
  nextCommitOrdinal: number,
): CompatibleNameOperationRowV1 {
  return Object.freeze({
    header,
    nextCommitOrdinal: boundedOrdinal(
      nextCommitOrdinal,
      false,
      'next compatible-name commit ordinal',
    ),
  })
}

export function storedOperationRow(
  header: CompatibleNameOperationHeaderV1,
  nextCommitOrdinal: number,
): StoredCompatibleNameOperationRowV1 {
  return Object.freeze({
    ...header,
    nextCommitOrdinal: boundedOrdinal(
      nextCommitOrdinal,
      false,
      'next compatible-name commit ordinal',
    ),
  })
}

export function headerWithPairOwnership(
  header: CompatibleNameOperationHeaderV1,
  pairKind: CompatibleNamePairKind,
): CompatibleNameOperationHeaderV1 {
  const pair = Object.freeze({
    ...header.pair,
    [pairKind]: Object.freeze({
      ...header.pair[pairKind],
      ownershipState: 'owned' as const,
    }),
  })
  const activationState = pair.script.ownershipState === 'owned' &&
      pair.sidecar.ownershipState === 'owned'
    ? 'pair-ready' as const
    : 'prepared' as const
  return replaceHeader(header, { pair, activationState })
}

export function headerAfterCommit(
  header: CompatibleNameOperationHeaderV1,
  mapping: CompatibleNameMappingV1,
): CompatibleNameOperationHeaderV1 {
  const prior = header.repairSummary
  const repairSummary = prior === undefined
    ? undefined
    : compatibleNameRepairSummary({
        ...prior,
        committedCount: (mapping.commitOrdinal ?? prior.committedCount),
        logicalPathSample: prior.logicalPathSample.length >=
            MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS ||
            prior.logicalPathSample.some(path => samePath(path, mapping.logicalPath))
          ? prior.logicalPathSample
          : [...prior.logicalPathSample, mapping.logicalPath]
              .slice(0, MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS),
        sidecarSync: 'pending',
      })
  return replaceHeader(header, {
    ...(repairSummary === undefined ? {} : { repairSummary }),
  })
}

export function replaceHeader(
  header: CompatibleNameOperationHeaderV1,
  changes: Partial<Omit<CompatibleNameOperationHeaderV1, 'formatVersion' | 'operationId'>>,
): CompatibleNameOperationHeaderV1 {
  const value = {
    ...header,
    ...changes,
  }
  return compatibleNameOperationHeaderV1(value)
}

export function headerWithoutPendingOutcome(
  header: CompatibleNameOperationHeaderV1,
  repairSummary: CompatibleNameRepairSummary,
): CompatibleNameOperationHeaderV1 {
  const withoutPending = { ...header, repairSummary }
  delete withoutPending.pendingTerminalOutcome
  return compatibleNameOperationHeaderV1(withoutPending)
}

export async function assertRepairSummary(
  transaction: IDBTransaction,
  operation: CompatibleNameOperationRowV1,
  summary: CompatibleNameRepairSummary,
): Promise<void> {
  const committedCount = operation.nextCommitOrdinal - 1
  if (summary.committedCount !== committedCount ||
      summary.placement !== operation.header.pairPlacement ||
      summary.pairDisplayNames.script !== operation.header.pair.script.physicalName ||
      summary.pairDisplayNames.sidecar !== operation.header.pair.sidecar.physicalName) {
    abortIntegrity(transaction, 'compatible-name repair summary disagrees with ledger truth')
  }
  const mappings = transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE)
  for (const logicalPath of summary.logicalPathSample) {
    const [fileValue, directoryValue] = await Promise.all([
      requestResult<unknown>(mappings.get(compatibleNameMappingId(
        operation.header.operationId,
        logicalPath,
        'file',
      ))),
      requestResult<unknown>(mappings.get(compatibleNameMappingId(
        operation.header.operationId,
        logicalPath,
        'directory',
      ))),
    ])
    const file = fileValue === undefined ? undefined : readMapping(fileValue)
    const directory = directoryValue === undefined ? undefined : readMapping(directoryValue)
    if (file?.commitState !== 'committed' && directory?.commitState !== 'committed') {
      abortIntegrity(transaction, 'repair summary samples an uncommitted logical path')
    }
  }
}

export function assertPairReady(
  transaction: IDBTransaction,
  header: CompatibleNameOperationHeaderV1,
): void {
  if (header.pair.script.ownershipState !== 'owned' ||
      header.pair.sidecar.ownershipState !== 'owned' || header.activationState !== 'active' ||
      header.repairSummary === undefined) {
    abortIntegrity(transaction, 'compatible target cannot commit before its restoration pair')
  }
}

export function assertPairOwned(
  transaction: IDBTransaction,
  header: CompatibleNameOperationHeaderV1,
): void {
  if (header.pair.script.ownershipState !== 'owned' ||
      header.pair.sidecar.ownershipState !== 'owned' || header.activationState === 'prepared') {
    abortIntegrity(transaction, 'compatible target cannot activate before its restoration pair')
  }
}

export function assertMappingCommitsOpen(
  transaction: IDBTransaction,
  header: CompatibleNameOperationHeaderV1,
): void {
  const footer = header.repairSummary?.latestObservedFooter
  if (header.pendingTerminalOutcome !== undefined ||
      (footer !== undefined && footer.state !== 'active')) {
    abortIntegrity(transaction, 'compatible-name mapping commit crossed the terminal cut')
  }
}

export function isTerminalRepairSummary(summary: CompatibleNameRepairSummary | undefined): boolean {
  if (summary === undefined) {
    return false
  }
  const footer = summary.latestObservedFooter
  return footer !== undefined && footer.state !== 'active' && summary.terminalSettlement === 'complete'
}

export function readPairHandle(value: unknown): ReceiveOperationHandleRecord<FileSystemFileHandle> {
  const record = validateReceiveOperationHandleRecord(
    value as ReceiveOperationHandleRecord<FileSystemFileHandle>,
  )
  requireFileHandle(record.handle)
  return record
}

export function samePairHandleMetadata(
  left: ReceiveOperationHandleRecord<FileSystemFileHandle>,
  right: ReceiveOperationHandleRecord<FileSystemFileHandle>,
): boolean {
  return left.id === right.id && left.operationId === right.operationId &&
    left.kind === right.kind && left.authorityRef === right.authorityRef &&
    left.ownedObjectId === right.ownedObjectId
}

export function mappingPhysicalClaimKey(
  header: CompatibleNameOperationHeaderV1,
  mapping: CompatibleNameMappingV1,
): string {
  return physicalClaimKey(
    compatibleMappingPhysicalParent(header, mapping.logicalPath),
    mapping.physicalComponent,
  )
}

export function pairPhysicalClaimKeys(header: CompatibleNameOperationHeaderV1): ReadonlySet<string> {
  const parent = compatiblePairPhysicalParent(header)
  return new Set([
    physicalClaimKey(parent, header.pair.script.physicalName),
    physicalClaimKey(parent, header.pair.sidecar.physicalName),
  ])
}

export function physicalClaimKey(parent: readonly string[], component: string): string {
  const parentKey = parent.map(value => `${value.length}:${value}`).join('/')
  return `${parentKey}\0${catalogNameCollisionKey(component)}`
}

export function sameCleanupAuthority(
  current: CompatibleNameOperationHeaderV1,
  expected: CompatibleNameOperationHeaderV1,
): boolean {
  return current.operationId === expected.operationId && current.authorityRef === expected.authorityRef &&
    current.root.logicalName === expected.root.logicalName &&
    current.root.physicalName === expected.root.physicalName &&
    current.pairPlacement === expected.pairPlacement &&
    current.pair.script.handleId === expected.pair.script.handleId &&
    current.pair.script.ownedObjectId === expected.pair.script.ownedObjectId &&
    current.pair.script.physicalName === expected.pair.script.physicalName &&
    current.pair.sidecar.handleId === expected.pair.sidecar.handleId &&
    current.pair.sidecar.ownedObjectId === expected.pair.sidecar.ownedObjectId &&
    current.pair.sidecar.physicalName === expected.pair.sidecar.physicalName
}

export function requireFileHandle(value: FileSystemFileHandle): FileSystemFileHandle {
  if (typeof value !== 'object' || value === null || value.kind !== 'file' ||
      typeof value.isSameEntry !== 'function') {
    throw new TypeError('restoration pair ownership requires a file handle')
  }
  return value
}

export function sameMappingSelection(
  left: CompatibleNameMappingV1,
  right: CompatibleNameMappingV1,
): boolean {
  return left.id === right.id && left.operationId === right.operationId &&
    left.entryKind === right.entryKind && samePath(left.logicalPath, right.logicalPath) &&
    left.physicalComponent === right.physicalComponent && left.attempt === right.attempt &&
    left.token === right.token
}

export function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

export function compatiblePairKind(value: CompatibleNamePairKind): CompatibleNamePairKind {
  if (value !== 'script' && value !== 'sidecar') {
    throw new TypeError('compatible-name restoration pair kind is invalid')
  }
  return value
}

export function boundedOrdinal(value: number, allowZero: boolean, label: string): number {
  const minimum = allowZero ? 0 : 1
  if (!Number.isSafeInteger(value) || value < minimum ||
      value > MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS + 1) {
    throw new TypeError(`${label} is invalid`)
  }
  return value
}

export function compareText(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

export function sameValue(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true
  if (typeof left !== 'object' || left === null ||
      typeof right !== 'object' || right === null) return false
  if (Array.isArray(left) || Array.isArray(right)) {
    return Array.isArray(left) && Array.isArray(right) && left.length === right.length &&
      left.every((value, index) => sameValue(value, right[index]))
  }
  const leftRecord = left as Record<string, unknown>
  const rightRecord = right as Record<string, unknown>
  const leftKeys = Object.keys(leftRecord).sort()
  const rightKeys = Object.keys(rightRecord).sort()
  return leftKeys.length === rightKeys.length &&
    leftKeys.every((key, index) => key === rightKeys[index] &&
      sameValue(leftRecord[key], rightRecord[key]))
}

export function abortIntegrity(transaction: IDBTransaction, message: string): never {
  transaction.abort()
  throw new TypeError(message)
}

export function abortCapacity(transaction: IDBTransaction): never {
  transaction.abort()
  throw new DOMException(COMPATIBLE_NAME_LEDGER_CAPACITY_ERROR, 'QuotaExceededError')
}

export function abortQuietly(transaction: IDBTransaction): void {
  try {
    transaction.abort()
  } catch {
    // Completion or an earlier abort already made all writes unreachable.
  }
}
