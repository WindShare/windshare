import { FaultScope, OutputFaultCode, TransferFaultError, outputFault } from '../../transfer/fault'
import type { FSADiskFingerprint } from '../store/types'
import {
  isMissingFSAEntry,
  readFSADiskFingerprint,
  removeDirectoryIfEmpty,
  removeVerifiedFSALeaf,
} from './fsa-cleanup'
import { SafeNameRegistry, decideCollisionName, deterministicSafeComponent } from './safe-name'

const MAX_COLLISION_ATTEMPTS = 1_000

export interface FSADirectory {
  readonly handle: FileSystemDirectoryHandle
  readonly physicalPath: readonly string[]
  readonly created: boolean
}

export type ReservedRootEntry =
  | Readonly<{ kind: 'directory'; handle: FileSystemDirectoryHandle }>
  | Readonly<{ kind: 'file'; handle: FileSystemFileHandle; fingerprint: FSADiskFingerprint }>

export interface ReservedFSATarget {
  readonly reservedName: string
  readonly collisionIndex: number
  readonly entry: ReservedRootEntry
}

export function requiresSafeComponent(error: unknown): boolean {
  return error instanceof TypeError || (error instanceof DOMException && error.name === 'TypeMismatchError')
}

export async function openDirectory(parent: FSADirectory, name: string): Promise<FSADirectory> {
  const existing = await parent.handle.getDirectoryHandle(name).catch((error: unknown) => {
    if (!isMissingFSAEntry(error)) throw error
  })
  const handle = existing ?? await parent.handle.getDirectoryHandle(name, { create: true })
  return { handle, physicalPath: [...parent.physicalPath, handle.name], created: existing === undefined }
}

export async function openFSAFile(
  parent: FileSystemDirectoryHandle,
  logicalPath: readonly string[],
  leaf: string,
  safeNames: SafeNameRegistry,
): Promise<{ handle: FileSystemFileHandle; created: boolean }> {
  const open = async (name: string) => {
    try {
      return { handle: await parent.getFileHandle(name), created: false }
    } catch (error) {
      if (!isMissingFSAEntry(error)) throw error
    }
    return { handle: await parent.getFileHandle(name, { create: true }), created: true }
  }
  try {
    return await open(leaf)
  } catch (error) {
    if (!requiresSafeComponent(error)) throw error
    const name = await deterministicSafeComponent({ logicalPath, entryKind: 'file' })
    const entry = await open(name)
    safeNames.record(logicalPath, 'file', name)
    return entry
  }
}

export async function reserveFSATarget(
  parentHandle: FileSystemDirectoryHandle,
  requestedName: string,
  singleFile: boolean,
  safeNames: SafeNameRegistry,
): Promise<ReservedFSATarget> {
  const kind = singleFile ? 'file' : 'directory'
  for (let collisionIndex = 0; collisionIndex < MAX_COLLISION_ATTEMPTS; collisionIndex++) {
    let reservedName = decideCollisionName(requestedName, kind, collisionIndex)
    let entry = await reserveRootEntry(parentHandle, reservedName, singleFile)
    if (entry === 'invalid-name') {
      reservedName = await deterministicSafeComponent({ logicalPath: [requestedName], entryKind: kind, collisionIndex })
      entry = await reserveRootEntry(parentHandle, reservedName, singleFile)
      if (typeof entry !== 'string') safeNames.recordRoot([requestedName], kind, reservedName)
    }
    if (typeof entry !== 'string') return { reservedName, collisionIndex, entry }
  }
  throw new TransferFaultError(
    outputFault(FaultScope.Job, OutputFaultCode.TargetCollision),
    'Could not allocate a unique name in the selected folder',
  )
}

export async function rollbackReservedTarget(
  parentHandle: FileSystemDirectoryHandle,
  reserved: ReservedFSATarget,
): Promise<void> {
  if (reserved.entry.kind === 'file') {
    await removeVerifiedFSALeaf(
      parentHandle,
      reserved.reservedName,
      reserved.entry.handle,
      reserved.entry.fingerprint,
    )
    return
  }
  await removeDirectoryIfEmpty(parentHandle, reserved.reservedName)
}

async function reserveRootEntry(
  parent: FileSystemDirectoryHandle,
  name: string,
  singleFile: boolean,
): Promise<ReservedRootEntry | 'exists' | 'invalid-name'> {
  const state = await entryExists(parent, name)
  if (state !== 'missing') return state
  try {
    if (!singleFile) {
      return { kind: 'directory', handle: await parent.getDirectoryHandle(name, { create: true }) }
    }
    const handle = await parent.getFileHandle(name, { create: true })
    const fingerprint = await readFSADiskFingerprint(handle)
    // An external writer can populate the same path after entryExists; never adopt non-empty bytes.
    if (fingerprint.size !== 0) return 'exists'
    return { kind: 'file', handle, fingerprint }
  } catch (error) {
    if (error instanceof TypeError) return 'invalid-name'
    if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'exists'
    throw error
  }
}

export async function entryExists(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<'missing' | 'exists' | 'invalid-name'> {
  const fileProbe = await probeHandle(() => parent.getFileHandle(name))
  if (fileProbe !== 'missing') return fileProbe
  return probeHandle(() => parent.getDirectoryHandle(name))
}

async function probeHandle(open: () => Promise<unknown>): Promise<'missing' | 'exists' | 'invalid-name'> {
  try {
    await open()
    return 'exists'
  } catch (error) {
    if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'exists'
    if (error instanceof TypeError) return 'invalid-name'
    if (isMissingFSAEntry(error)) return 'missing'
    throw error
  }
}
