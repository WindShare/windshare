import { BrowserFileSystemTree } from '../../src/output/browser/filesystem-tree'
import { IndexedDbOutputRepository } from '../../src/output/browser/indexeddb-repository'
import {
  acquireBrowserFileSystemAccessSessionLease,
  BrowserOutputSessionBusyError,
  type BrowserFileSystemAccessSessionLease,
} from '../../src/output/browser/session-lease'
import { FILE_SYSTEM_ACCESS_BACKEND } from '../../src/output/capability/contract'

const SHARED_FILE = 'shared.bin'
const SHARED_DIRECTORY = 'shared-directory'

export interface FsaNamespaceFixture {
  readonly databaseName: string
  readonly rootName: string
  readonly rootIdentity: string
  readonly firstIntent: string
  readonly secondIntent: string
}

export interface FsaNamespaceOwnership {
  readonly fileIdentity: string
  readonly directoryIdentity: string
}

interface HeldNamespace {
  readonly repository: IndexedDbOutputRepository
  readonly lease: BrowserFileSystemAccessSessionLease
}

const heldNamespaces = new Map<string, HeldNamespace>()

export async function holdFirstIntent(
  fixture: FsaNamespaceFixture,
): Promise<FsaNamespaceOwnership> {
  if (heldNamespaces.has(fixture.databaseName)) {
    throw new Error('The test namespace is already held')
  }
  const root = await sharedRoot(fixture, true)
  const repository = await IndexedDbOutputRepository.open(
    fixture.databaseName,
    namespace(fixture, fixture.firstIntent),
  )
  let lease: BrowserFileSystemAccessSessionLease | undefined
  try {
    lease = await acquireBrowserFileSystemAccessSessionLease(repository.binding)
    const tree = BrowserFileSystemTree.forSharedRoot({
      root,
      handles: repository,
      mutations: lease.mutations,
    })
    const file = await tree.createFileExclusive([SHARED_FILE])
    const directory = await tree.ensureDirectory([SHARED_DIRECTORY])
    if (!directory.created) throw new Error('The first intent did not create its directory')
    heldNamespaces.set(fixture.databaseName, { repository, lease })
    return Object.freeze({
      fileIdentity: file.identity,
      directoryIdentity: directory.identity,
    })
  } catch (error) {
    repository.close()
    await lease?.release().catch(() => undefined)
    throw error
  }
}

export async function probeCompetingIntent(
  fixture: FsaNamespaceFixture,
): Promise<{ readonly name: string; readonly scope?: string }> {
  const repository = await IndexedDbOutputRepository.open(
    fixture.databaseName,
    namespace(fixture, fixture.secondIntent),
  )
  let lease: BrowserFileSystemAccessSessionLease | undefined
  try {
    lease = await acquireBrowserFileSystemAccessSessionLease(repository.binding)
    return { name: 'acquired' }
  } catch (error) {
    return error instanceof BrowserOutputSessionBusyError
      ? { name: error.name, scope: error.scope }
      : { name: error instanceof Error ? error.name : 'Error' }
  } finally {
    repository.close()
    await lease?.release().catch(() => undefined)
  }
}

export async function verifyRecoveryAndOwnership(
  fixture: FsaNamespaceFixture,
  ownership: FsaNamespaceOwnership,
): Promise<{
  readonly fileCreateError: string
  readonly directoryCreated: boolean
  readonly fileRemoveError: string
  readonly directoryRemoveError: string
  readonly filePreserved: boolean
  readonly directoryPreserved: boolean
}> {
  const originRoot = await originPrivateRoot()
  const root = await sharedRoot(fixture, false)
  const originalFile = await root.getFileHandle(SHARED_FILE)
  const originalDirectory = await root.getDirectoryHandle(SHARED_DIRECTORY)
  const repository = await IndexedDbOutputRepository.open(
    fixture.databaseName,
    namespace(fixture, fixture.secondIntent),
  )
  let lease: BrowserFileSystemAccessSessionLease | undefined
  try {
    lease = await acquireBrowserFileSystemAccessSessionLease(repository.binding)
    const tree = BrowserFileSystemTree.forSharedRoot({
      root,
      handles: repository,
      mutations: lease.mutations,
    })
    const fileCreateError = await errorName(() => tree.createFileExclusive([SHARED_FILE]))
    const directory = await tree.ensureDirectory([SHARED_DIRECTORY])
    const fileRemoveError = await errorName(() => tree.removeFile(
      [SHARED_FILE],
      ownership.fileIdentity,
    ))
    const directoryRemoveError = await errorName(() => tree.removeDirectory(
      [SHARED_DIRECTORY],
      ownership.directoryIdentity,
    ))
    const currentFile = await root.getFileHandle(SHARED_FILE)
    const currentDirectory = await root.getDirectoryHandle(SHARED_DIRECTORY)
    return Object.freeze({
      fileCreateError,
      directoryCreated: directory.created,
      fileRemoveError,
      directoryRemoveError,
      filePreserved: await currentFile.isSameEntry(originalFile),
      directoryPreserved: await currentDirectory.isSameEntry(originalDirectory),
    })
  } finally {
    repository.close()
    await lease?.release().catch(() => undefined)
    await originRoot.removeEntry(fixture.rootName, { recursive: true }).catch(() => undefined)
    await deleteDatabase(fixture.databaseName)
  }
}

async function sharedRoot(
  fixture: FsaNamespaceFixture,
  create: boolean,
): Promise<FileSystemDirectoryHandle> {
  const root = await originPrivateRoot()
  return root.getDirectoryHandle(fixture.rootName, create ? { create: true } : undefined)
}

async function originPrivateRoot(): Promise<FileSystemDirectoryHandle> {
  const storage = navigator.storage as StorageManager & {
    getDirectory(): Promise<FileSystemDirectoryHandle>
  }
  return storage.getDirectory()
}

function namespace(fixture: FsaNamespaceFixture, transferIntentDigest: string) {
  return Object.freeze({
    backend: FILE_SYSTEM_ACCESS_BACKEND,
    transferIntentDigest,
    rootIdentity: fixture.rootIdentity,
  })
}

async function errorName(operation: () => Promise<unknown>): Promise<string> {
  try {
    await operation()
    return 'none'
  } catch (error) {
    return error instanceof Error ? error.name : 'Error'
  }
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('IndexedDB deletion failed'))
    request.onblocked = () => reject(new Error('IndexedDB deletion was blocked'))
  })
}
