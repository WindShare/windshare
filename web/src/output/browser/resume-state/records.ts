import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  canonicalFileCheckpointBackend,
  identityBytes,
} from '../../persistence/checkpoint'
import {
  outputRecordKey,
  type PersistedDirectoryRecord,
  type PersistedFileRecord,
  type PersistedOutputRecord,
} from '../../persistence/journal'
import {
  durableCheckpointNamespaceIdentity,
  durableCheckpointNamespaceKey,
} from '../../persistence/namespace'
import {
  pausedTaskDescriptorKey,
  pausedTaskDescriptorNamespace,
  samePausedTaskDescriptor,
  snapshotRootCapabilityRef,
  validatePausedTaskDescriptorV1,
  type PausedTaskDescriptorV1,
} from '../../resume/descriptor'
import { ORIGIN_PRIVATE_BACKEND } from '../../capability/contract'
import type { BrowserFileSystemTree } from '../filesystem-tree'
import {
  INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
  INDEXEDDB_ROOT_CAPABILITY_STORE,
  isIndexedDbRecord,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from '../indexeddb-database'
import { verifyIndexedDbRootIdentity } from '../indexeddb-root-binding'

const CAPABILITY_SCHEMA_VERSION = 1 as const

interface StoredPausedTaskDescriptor {
  readonly id: string
  readonly descriptor: PausedTaskDescriptorV1
}

export interface StoredRootCapability {
  readonly id: string
  readonly schemaVersion: typeof CAPABILITY_SCHEMA_VERSION
  readonly namespace: string
  readonly backend: string
  readonly rootIdentity: string
  readonly handle: FileSystemDirectoryHandle
}

interface StoredNamespaceMetadata {
  readonly id: string
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespaceName: typeof FILE_CHECKPOINT_NAMESPACE
  readonly backend: string
  readonly transferIntentDigest: string
  readonly rootIdentity: string
  readonly state: 'active' | 'cleanup-pending'
  readonly cleanupStep: number
}

export type PausedTaskCapabilityFailure =
  | 'missing'
  | 'binding-mismatch'
  | 'permission-denied'
  | 'stale'
  | 'unsupported'

export class PausedTaskCapabilityError extends Error {
  readonly failure: PausedTaskCapabilityFailure

  constructor(failure: PausedTaskCapabilityFailure, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'PausedTaskCapabilityError'
    this.failure = failure
  }
}

export class PausedTaskDescriptorConflictError extends Error {
  constructor() {
    super('A paused task already owns this transfer intent and output root namespace')
    this.name = 'PausedTaskDescriptorConflictError'
  }
}

export function storedPausedTaskDescriptor(
  descriptor: PausedTaskDescriptorV1,
): StoredPausedTaskDescriptor {
  return Object.freeze({
    id: pausedTaskDescriptorKey(descriptor),
    descriptor,
  })
}

export function storedRootCapability(
  descriptor: PausedTaskDescriptorV1,
  root: FileSystemDirectoryHandle,
): StoredRootCapability {
  const namespace = pausedTaskDescriptorNamespace(descriptor)
  return Object.freeze({
    id: descriptor.rootCapabilityRef,
    schemaVersion: CAPABILITY_SCHEMA_VERSION,
    namespace: durableCheckpointNamespaceKey(namespace),
    backend: namespace.backend,
    rootIdentity: namespace.rootIdentity,
    handle: root,
  })
}

export type IndexedDbResumeStateDiscardPhase = 'retiring' | 'exporting' | 'exported'

export interface IndexedDbResumeStateDiscardMarker {
  readonly descriptorKey: string
  readonly rootCapabilityRef: string
  readonly backend: string
  readonly inventoryDigest: string
  readonly phase: IndexedDbResumeStateDiscardPhase
}

interface StoredResumeStateDiscardMarker extends IndexedDbResumeStateDiscardMarker {
  readonly id: string
  readonly namespace: string
  readonly schemaVersion: 1
}

export function snapshotResumeStateDiscardMarker(
  value: unknown,
  namespace: string,
): IndexedDbResumeStateDiscardMarker {
  if (!isIndexedDbRecord(value) || value.id !== namespace || value.namespace !== namespace ||
      value.schemaVersion !== 1 || value.descriptorKey !== namespace ||
      typeof value.rootCapabilityRef !== 'string' || value.rootCapabilityRef.length === 0 ||
      typeof value.backend !== 'string' ||
      canonicalFileCheckpointBackend(value.backend) !== value.backend ||
      typeof value.inventoryDigest !== 'string' ||
      !['retiring', 'exporting', 'exported'].includes(String(value.phase))) {
    throw new Error('IndexedDB resume-state discard journal is invalid')
  }
  identityBytes(value.inventoryDigest, 32, 'resume-state discard inventory digest')
  return Object.freeze({
    descriptorKey: value.descriptorKey,
    rootCapabilityRef: value.rootCapabilityRef,
    backend: value.backend,
    inventoryDigest: value.inventoryDigest,
    phase: value.phase as IndexedDbResumeStateDiscardPhase,
  })
}

export function storedResumeStateDiscardMarker(
  marker: IndexedDbResumeStateDiscardMarker,
  namespace: string,
): StoredResumeStateDiscardMarker {
  return Object.freeze({
    ...marker,
    id: namespace,
    namespace,
    schemaVersion: 1,
  })
}

export function sameResumeStateDiscardMarker(
  left: IndexedDbResumeStateDiscardMarker,
  right: IndexedDbResumeStateDiscardMarker,
): boolean {
  return left.descriptorKey === right.descriptorKey &&
    left.rootCapabilityRef === right.rootCapabilityRef &&
    left.backend === right.backend &&
    left.inventoryDigest === right.inventoryDigest &&
    left.phase === right.phase
}

export async function preflightResumeState(
  tree: BrowserFileSystemTree,
  committed: readonly PersistedOutputRecord[],
  candidates: readonly PersistedOutputRecord[],
  discardMarker: IndexedDbResumeStateDiscardMarker | undefined,
): Promise<boolean> {
  const records = effectivePinnedResumeRecords(committed, candidates)
  if (records === undefined) return false
  const preservedFiles = completedFiles(committed)
  for (const record of records) {
    if (!await resumeStateRecordMatches(tree, record, discardMarker, preservedFiles)) return false
  }
  return true
}

function effectivePinnedResumeRecords(
  committed: readonly PersistedOutputRecord[],
  candidates: readonly PersistedOutputRecord[],
): readonly PersistedOutputRecord[] | undefined {
  const committedByKey = new Map(committed.map((record) => [outputRecordKey(record), record]))
  const effective = [...committed]
  for (const candidate of candidates) {
    const published = committedByKey.get(outputRecordKey(candidate))
    if (published === undefined) effective.push(candidate)
    else if (!samePhysicalObject(candidate, published)) return undefined
  }
  return effective
}

async function resumeStateRecordMatches(
  tree: BrowserFileSystemTree,
  record: PersistedOutputRecord,
  discardMarker: IndexedDbResumeStateDiscardMarker | undefined,
  preservedFiles: readonly PersistedFileRecord[],
): Promise<boolean> {
  if (record.kind === 'directory') {
    const state = await tree.ownedDirectoryState(
      record.canonicalPath,
      record.ownedDirectoryIdentity,
    )
    return state === 'owned' || state === 'absent' &&
      recordMayBeAbsentAfterDiscard(record, discardMarker, preservedFiles)
  }
  const state = await tree.ownedFileState(record.canonicalPath, record.ownedFileIdentity)
  if (state === 'absent') {
    return recordMayBeAbsentAfterDiscard(record, discardMarker, preservedFiles)
  }
  if (state !== 'owned') return false
  const file = await tree.openFile(record.canonicalPath, record.ownedFileIdentity)
  if (file === undefined) return false
  try {
    const size = await file.size()
    return record.committed
      ? size === record.exactSize
      : size <= record.exactSize && size >= durableHighWaterMark(record)
  } finally {
    await file.close()
  }
}

function recordMayBeAbsentAfterDiscard(
  record: PersistedOutputRecord,
  marker: IndexedDbResumeStateDiscardMarker | undefined,
  preservedFiles: readonly PersistedFileRecord[],
): boolean {
  if (marker === undefined) return false
  if (marker.backend === ORIGIN_PRIVATE_BACKEND) return marker.phase !== 'exporting'
  if (record.kind === 'file') return !record.committed
  return record.createdBySession && !preservedFiles.some((file) =>
    pathStartsWith(file.canonicalPath, record.canonicalPath))
}

export async function discardFileSystemAccessObjects(
  tree: BrowserFileSystemTree,
  committed: readonly PersistedOutputRecord[],
  candidates: readonly PersistedOutputRecord[],
): Promise<void> {
  const committedByKey = new Map(committed.map((record) => [outputRecordKey(record), record]))
  const candidateOnly = candidates.filter((record) => !committedByKey.has(outputRecordKey(record)))
  const effective = [...committed, ...candidateOnly]
  const preservedFiles = completedFiles(committed)
  for (const record of effective) {
    if (record.kind === 'file' && !record.committed) {
      await tree.removeFile(record.canonicalPath, record.ownedFileIdentity)
    }
  }
  const directories = effective
    .filter((record): record is PersistedDirectoryRecord => record.kind === 'directory')
    .sort((left, right) => right.canonicalPath.length - left.canonicalPath.length)
  for (const directory of directories) {
    const preservesCompletedFile = preservedFiles.some((file) =>
      pathStartsWith(file.canonicalPath, directory.canonicalPath))
    if (directory.createdBySession && !preservesCompletedFile) {
      await tree.removeDirectory(directory.canonicalPath, directory.ownedDirectoryIdentity)
    }
  }
}

export function completedFiles(
  records: readonly PersistedOutputRecord[],
): readonly PersistedFileRecord[] {
  return records.filter(
    (record): record is PersistedFileRecord => record.kind === 'file' && record.committed,
  )
}

function samePhysicalObject(
  left: PersistedOutputRecord,
  right: PersistedOutputRecord,
): boolean {
  if (left.kind !== right.kind) return false
  return left.kind === 'file' && right.kind === 'file'
    ? left.ownedFileIdentity === right.ownedFileIdentity
    : left.kind === 'directory' && right.kind === 'directory' &&
      left.ownedDirectoryIdentity === right.ownedDirectoryIdentity
}

function durableHighWaterMark(record: PersistedFileRecord): bigint {
  return record.durableRanges.reduce(
    (maximum, range) => range.end > maximum ? range.end : maximum,
    0n,
  )
}

function pathStartsWith(path: readonly string[], prefix: readonly string[]): boolean {
  return prefix.length < path.length &&
    prefix.every((segment, index) => path[index] === segment)
}

export async function readCurrentResumeState(
  databaseName: string,
  descriptor: PausedTaskDescriptorV1,
): Promise<StoredRootCapability | undefined> {
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const key = pausedTaskDescriptorKey(descriptor)
    const transaction = database.transaction([
      INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
      INDEXEDDB_ROOT_CAPABILITY_STORE,
    ], 'readonly')
    const [storedDescriptor, storedCapability] = await Promise.all([
      requestResult<unknown>(
        transaction.objectStore(INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE).get(key),
      ),
      requestResult<unknown>(
        transaction.objectStore(INDEXEDDB_ROOT_CAPABILITY_STORE)
          .get(descriptor.rootCapabilityRef),
      ),
    ])
    await transactionCompletion(transaction)
    if (storedDescriptor === undefined && storedCapability === undefined) return undefined
    if (storedDescriptor === undefined || storedCapability === undefined) {
      throw new PausedTaskCapabilityError(
        'binding-mismatch',
        'Paused task descriptor and root capability were not settled atomically',
      )
    }
    const current = await validateStoredDescriptor(storedDescriptor)
    if (!samePausedTaskDescriptor(current, descriptor)) {
      throw new PausedTaskCapabilityError(
        'binding-mismatch',
        'Paused task changed after inventory',
      )
    }
    return validateStoredCapability(storedCapability, descriptor)
  } finally {
    database.close()
  }
}

export async function sameDirectoryEntry(
  current: FileSystemDirectoryHandle,
  expected: FileSystemDirectoryHandle,
): Promise<boolean> {
  try {
    return await current.isSameEntry(expected)
  } catch (error) {
    throw new PausedTaskCapabilityError(
      'stale',
      'The output root capability could not be compared safely',
      { cause: error },
    )
  }
}

export async function verifyPreparedRootIdentity(
  databaseName: string,
  backend: string,
  rootIdentity: string,
  root: FileSystemDirectoryHandle,
): Promise<void> {
  try {
    await verifyIndexedDbRootIdentity({ databaseName, backend, rootIdentity, root })
  } catch (error) {
    throw new PausedTaskCapabilityError(
      'stale',
      'The persisted root capability no longer proves the paused task namespace',
      { cause: error },
    )
  }
}

export async function assertPreparedCapabilityCurrent(
  databaseName: string,
  descriptor: PausedTaskDescriptorV1,
  prepared: StoredRootCapability,
): Promise<void> {
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const current = await readPreparedCapability(database, descriptor)
    let sameEntry: boolean
    try {
      sameEntry = await current.handle.isSameEntry(prepared.handle)
    } catch (error) {
      throw new PausedTaskCapabilityError(
        'stale',
        'The paused task root capability could not be revalidated',
        { cause: error },
      )
    }
    if (!sameEntry) {
      throw new PausedTaskCapabilityError(
        'stale',
        'The paused task root capability was replaced after preparation',
      )
    }
  } finally {
    database.close()
  }
}

export async function readPreparedCapability(
  database: IDBDatabase,
  descriptor: PausedTaskDescriptorV1,
): Promise<StoredRootCapability> {
  const key = pausedTaskDescriptorKey(descriptor)
  const transaction = database.transaction([
    INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
    INDEXEDDB_ROOT_CAPABILITY_STORE,
  ], 'readonly')
  const [storedDescriptor, storedCapability] = await Promise.all([
    requestResult<unknown>(
      transaction.objectStore(INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE).get(key),
    ),
    requestResult<unknown>(
      transaction.objectStore(INDEXEDDB_ROOT_CAPABILITY_STORE)
        .get(descriptor.rootCapabilityRef),
    ),
  ])
  await transactionCompletion(transaction)
  if (storedDescriptor === undefined) {
    throw new PausedTaskCapabilityError('missing', 'Paused task descriptor is unavailable')
  }
  const current = await validateStoredDescriptor(storedDescriptor)
  if (!samePausedTaskDescriptor(current, descriptor)) {
    throw new PausedTaskCapabilityError(
      'binding-mismatch',
      'Paused task descriptor does not match its stored namespace',
    )
  }
  return validateStoredCapability(storedCapability, descriptor)
}

export async function validateStoredDescriptor(
  value: unknown,
): Promise<PausedTaskDescriptorV1> {
  const record = requireExactRecord(value, ['id', 'descriptor'], 'stored paused task')
  const descriptor = await validatePausedTaskDescriptorV1(record.descriptor)
  if (record.id !== pausedTaskDescriptorKey(descriptor)) {
    throw new PausedTaskCapabilityError(
      'binding-mismatch',
      'Stored paused task key does not match its canonical namespace',
    )
  }
  return descriptor
}

export function validateStoredCapability(
  value: unknown,
  descriptor: PausedTaskDescriptorV1,
): StoredRootCapability {
  if (value === undefined) {
    throw new PausedTaskCapabilityError(
      'missing',
      'The paused task root capability is unavailable',
    )
  }
  const record = requireExactRecord(
    value,
    ['id', 'schemaVersion', 'namespace', 'backend', 'rootIdentity', 'handle'],
    'stored root capability',
  )
  const handle = record.handle as FileSystemDirectoryHandle | undefined
  const namespace = pausedTaskDescriptorNamespace(descriptor)
  if (record.schemaVersion !== CAPABILITY_SCHEMA_VERSION ||
      record.id !== snapshotRootCapabilityRef(descriptor.rootCapabilityRef) ||
      record.namespace !== durableCheckpointNamespaceKey(namespace) ||
      record.backend !== namespace.backend ||
      record.rootIdentity !== namespace.rootIdentity ||
      handle?.kind !== 'directory') {
    throw new PausedTaskCapabilityError(
      'binding-mismatch',
      'Stored root capability does not match the paused task',
    )
  }
  return record as unknown as StoredRootCapability
}

export function assertNamespaceMetadata(
  value: unknown,
  namespaceKey: string,
  descriptor: PausedTaskDescriptorV1,
): void {
  if (value === undefined) {
    throw new DOMException(
      'Paused tasks can be persisted only after the durable namespace is initialized',
      'InvalidStateError',
    )
  }
  const record = requireExactRecord(
    value,
    [
      'id',
      'marker',
      'namespaceName',
      'backend',
      'transferIntentDigest',
      'rootIdentity',
      'state',
      'cleanupStep',
    ],
    'checkpoint namespace metadata',
  ) as unknown as StoredNamespaceMetadata
  const namespace = durableCheckpointNamespaceIdentity(
    pausedTaskDescriptorNamespace(descriptor),
  )
  if (record.id !== namespaceKey ||
      record.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      record.namespaceName !== FILE_CHECKPOINT_NAMESPACE ||
      record.backend !== namespace.backend ||
      record.transferIntentDigest !== namespace.transferIntentDigest ||
      record.rootIdentity !== namespace.rootIdentity ||
      record.state !== 'active' ||
      record.cleanupStep !== 0) {
    throw new PausedTaskCapabilityError(
      'binding-mismatch',
      'Checkpoint namespace metadata does not match the paused task',
    )
  }
}

function requireExactRecord(
  value: unknown,
  keys: readonly string[],
  label: string,
): Record<string, unknown> {
  if (!isIndexedDbRecord(value)) {
    throw new PausedTaskCapabilityError('binding-mismatch', `${label} is not an object`)
  }
  const actual = Object.keys(value).sort()
  const expected = [...keys].sort()
  if (actual.length !== expected.length ||
      actual.some((key, index) => key !== expected[index])) {
    throw new PausedTaskCapabilityError(
      'binding-mismatch',
      `${label} has an invalid structured shape`,
    )
  }
  return value
}
