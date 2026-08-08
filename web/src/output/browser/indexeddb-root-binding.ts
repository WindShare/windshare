import { decodeBase64Url, encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  canonicalFileCheckpointBackend,
} from '../persistence/checkpoint'
import {
  createOutputCapabilityIdentity,
  snapshotOutputCapabilityIdentity,
  type OutputCapabilityIdentity,
} from '../capability/contract'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_CHECKPOINT_METADATA_STORE,
  isIndexedDbRecord,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'

const ROOT_BINDING_PREFIX =
  `${FILE_CHECKPOINT_OWNERSHIP_MARKER}\0${FILE_CHECKPOINT_NAMESPACE}\0root-binding\0`
const ROOT_BINDING_BUCKET_LIMIT = 128
const ROOT_BINDING_INSTALL_RETRY_LIMIT = 32

/**
 * The lexical bucket is only an index. Every returned identity still requires
 * the browser's same-entry relation over the persisted directory capability.
 */
interface RootBindingEntry {
  readonly rootIdentity: string
  readonly handle: FileSystemDirectoryHandle
}

interface RootBindingMetadata {
  readonly id: string
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespaceName: typeof FILE_CHECKPOINT_NAMESPACE
  readonly backend: string
  readonly rootName: string
  readonly revision: number
  readonly entries: readonly RootBindingEntry[]
}

/**
 * Resolve the stable identity for a picker-issued root.  The handle comparison
 * is performed before an identity is accepted; a name, path, or caller-supplied
 * label is never treated as proof of ownership.  The first open writes the
 * binding in the WindShare metadata store, and every later open verifies the
 * persisted handle with the browser's capability relation.
 */
export async function resolveIndexedDbRootIdentity(options: {
  readonly databaseName?: string
  readonly backend: string
  readonly root: FileSystemDirectoryHandle
}): Promise<OutputCapabilityIdentity> {
  const databaseName = options.databaseName ?? DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME
  const backend = canonicalFileCheckpointBackend(options.backend)
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const rootName = requireRootName(options.root)
    for (let attempt = 0; attempt < ROOT_BINDING_INSTALL_RETRY_LIMIT; attempt += 1) {
      const existing = await readRootBindingBucket(database, backend, rootName)
      const matched = await matchingRootBinding(existing?.entries ?? [], options.root)
      if (matched !== undefined) return matched
      if (existing !== undefined && existing.entries.length >= ROOT_BINDING_BUCKET_LIMIT) {
        throw new DOMException('Too many output roots share this browser name', 'QuotaExceededError')
      }

      // A random identity is generated only after every existing binding has
      // failed the browser's same-entry check.  The persisted handle is the
      // durable proof used by future tabs; no lexical/path fallback is possible.
      const identity = createOutputCapabilityIdentity()
      const candidate: RootBindingEntry = {
        rootIdentity: encodeBase64Url(identity),
        handle: options.root,
      }
      if (!await installRootBindingEntry(database, backend, rootName, existing, candidate)) continue

      // Verify the committed handle before returning its identity.  The
      // optimistic revision check in installRootBindingEntry makes concurrent
      // tabs converge on one bucket revision rather than minting namespaces.
      const persisted = await readRootBindingBucket(database, backend, rootName)
      const verified = await matchingRootBinding(persisted?.entries ?? [], options.root)
      if (verified !== undefined) return verified
    }
    throw new DOMException('The output root identity could not be installed safely', 'InvalidStateError')
  } finally {
    database.close()
  }
}

/** Read-only proof that a persisted capability still names the certified root. */
export async function verifyIndexedDbRootIdentity(options: {
  readonly databaseName?: string
  readonly backend: string
  readonly rootIdentity: string
  readonly root: FileSystemDirectoryHandle
}): Promise<void> {
  const databaseName = options.databaseName ?? DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME
  const backend = canonicalFileCheckpointBackend(options.backend)
  const expected = decodeBase64Url(options.rootIdentity)
  if (expected === undefined || encodeBase64Url(expected) !== options.rootIdentity) {
    throw new TypeError('persisted root identity is not canonical')
  }
  snapshotOutputCapabilityIdentity(expected, 'persisted root identity')
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const bucket = await readRootBindingBucket(database, backend, requireRootName(options.root))
    const matched = await matchingRootBinding(bucket?.entries ?? [], options.root)
    if (matched === undefined || !equalBytes(matched, expected)) {
      throw new DOMException('The persisted output root capability is stale', 'InvalidStateError')
    }
  } finally {
    database.close()
  }
}

async function readRootBindingBucket(
  database: IDBDatabase,
  backend: string,
  rootName: string,
): Promise<RootBindingMetadata | undefined> {
  const transaction = database.transaction(INDEXEDDB_CHECKPOINT_METADATA_STORE, 'readonly')
  const raw = await requestResult<unknown>(
    transaction.objectStore(INDEXEDDB_CHECKPOINT_METADATA_STORE).get(rootBindingKey(backend, rootName)),
  )
  await transactionCompletion(transaction)
  if (raw === undefined) return undefined
  if (!isIndexedDbRecord(raw)) throw new Error('IndexedDB root binding metadata is not an object')
  const bucket = raw as unknown as RootBindingMetadata
  validateRootBinding(bucket, backend, rootName)
  return bucket
}

async function matchingRootBinding(
  entries: readonly RootBindingEntry[],
  root: FileSystemDirectoryHandle,
): Promise<OutputCapabilityIdentity | undefined> {
  for (const entry of entries) {
    if (entry.handle.kind !== 'directory') {
      throw new Error('IndexedDB root binding is not a directory capability')
    }
    if (!await root.isSameEntry(entry.handle)) continue
    const identity = decodeBase64Url(entry.rootIdentity)
    if (identity === undefined) throw new Error('IndexedDB root binding identity is not canonical')
    return snapshotOutputCapabilityIdentity(identity, 'persisted root identity')
  }
  return undefined
}

async function installRootBindingEntry(
  database: IDBDatabase,
  backend: string,
  rootName: string,
  expected: RootBindingMetadata | undefined,
  candidate: RootBindingEntry,
): Promise<boolean> {
  const transaction = database.transaction(INDEXEDDB_CHECKPOINT_METADATA_STORE, 'readwrite')
  const store = transaction.objectStore(INDEXEDDB_CHECKPOINT_METADATA_STORE)
  const currentRaw = await requestResult<unknown>(store.get(rootBindingKey(backend, rootName)))
  if (currentRaw !== undefined && !isIndexedDbRecord(currentRaw)) {
    transaction.abort()
    throw new Error('IndexedDB root binding metadata is not an object')
  }
  const current = currentRaw === undefined ? undefined : currentRaw as unknown as RootBindingMetadata
  if (current !== undefined) validateRootBinding(current, backend, rootName)
  const expectedRevision = expected?.revision ?? 0
  const currentRevision = current?.revision ?? 0
  if (currentRevision !== expectedRevision) {
    await transactionCompletion(transaction)
    return false
  }
  const entries = current === undefined ? [] : [...current.entries]
  entries.push(candidate)
  const next: RootBindingMetadata = {
    id: rootBindingKey(backend, rootName),
    marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespaceName: FILE_CHECKPOINT_NAMESPACE,
    backend,
    rootName,
    revision: currentRevision + 1,
    entries: Object.freeze(entries),
  }
  store.put(next)
  await transactionCompletion(transaction)
  return true
}

function validateRootBinding(value: RootBindingMetadata, backend: string, rootName: string): void {
  if (value.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      value.namespaceName !== FILE_CHECKPOINT_NAMESPACE ||
      value.backend !== backend ||
      value.rootName !== rootName ||
      value.id !== rootBindingKey(backend, rootName) ||
      !Number.isSafeInteger(value.revision) || value.revision < 1 ||
      !Array.isArray(value.entries) || value.entries.length > ROOT_BINDING_BUCKET_LIMIT) {
    throw new Error('IndexedDB root binding ownership is invalid')
  }
  for (const entry of value.entries) {
    const handle = (entry as unknown as { readonly handle?: FileSystemHandle }).handle
    if (!isIndexedDbRecord(entry) || typeof entry.rootIdentity !== 'string' ||
        handle === undefined || handle.kind !== 'directory') {
      throw new Error('IndexedDB root binding entry is invalid')
    }
    const identity = decodeBase64Url(entry.rootIdentity)
    if (identity === undefined) throw new Error('IndexedDB root binding identity is not canonical')
    snapshotOutputCapabilityIdentity(identity, 'persisted root identity')
  }
}

function rootBindingPrefix(backend: string): string {
  return `${ROOT_BINDING_PREFIX}${backend}\0`
}

function rootBindingKey(backend: string, rootName: string): string {
  return `${rootBindingPrefix(backend)}${encodeBase64Url(new TextEncoder().encode(rootName))}`
}

function requireRootName(root: FileSystemDirectoryHandle): string {
  if (typeof root.name !== 'string') throw new TypeError('output root handle has no stable name')
  return root.name
}
