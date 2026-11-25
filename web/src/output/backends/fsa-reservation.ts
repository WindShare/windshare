import { FaultScope, OutputFaultCode, TransferFaultError, outputFault } from '../../transfer/fault'
import type { ReceiveLifecycleState } from '../../transfer/state'
import { foldPortableName15 } from '../../unicode/case-fold'
import type { ReceiveStore } from '../store/database'
import type { FSADiskFingerprint, FSATargetRecord } from '../store/types'
import {
  isMissingFSAEntry,
  readFSADiskFingerprint,
  removeDirectoryIfEmpty,
  removeVerifiedFSALeaf,
  sameFSAEntry,
} from './fsa-cleanup'
import { SafeNameRegistry, decideCollisionName, deterministicSafeComponent } from './safe-name'

const MAX_COLLISION_ATTEMPTS = 1_000
const EMPTY_CLAIMED_NAMES: ReadonlySet<string> = new Set()

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
  readonly skippedClaimedNames: number
  readonly entry: ReservedRootEntry
}

/**
 * Whether a persisted FSA task can still reopen, write to, or discard its
 * top-level `reservedName`. Completed saves and settled terminal results with
 * no deferred cleanup never touch the reserved slot again.
 */
export function retainsFSAReservationClaim(lifecycle: ReceiveLifecycleState): boolean {
  switch (lifecycle.kind) {
    case 'preparing':
    case 'downloading':
    case 'paused':
      return true
    case 'failed':
    case 'partial':
      return lifecycle.cleanupPending === true
    case 'saved':
    case 'ready-to-save':
    case 'download-started':
      return false
  }
}

/**
 * Collects normalized top-level reserved names held by unfinished FSA tasks in
 * `parentHandle` (mirroring Go's `existingTopLevelClaims`). Checking persisted
 * reservations prevents another file or folder task from claiming a slot whose
 * paused or cleanup-pending owner had its disk entry removed externally.
 */
export async function collectFSATopLevelClaims(
  store: Pick<ReceiveStore, 'listTasks'>,
  parentHandle: FileSystemDirectoryHandle,
  excludeTaskId?: string,
): Promise<ReadonlySet<string>> {
  const tasks = await store.listTasks()
  const claimed = new Set<string>()
  for (const task of tasks) {
    if (task.taskId === excludeTaskId || task.target.kind !== 'fsa') continue
    if (!retainsFSAReservationClaim(task.lifecycle)) continue
    if (!await sameFSAEntry(parentHandle, task.target.parentHandle).catch(() => false)) continue
    claimed.add(foldPortableName15(task.target.reservedName))
  }
  return claimed
}

export function buildFSATargetRecord(
  parentHandle: FileSystemDirectoryHandle,
  reserved: ReservedFSATarget,
  reservedAtMs: number,
): FSATargetRecord {
  if (reserved.entry.kind === 'file') {
    return Object.freeze({
      kind: 'fsa',
      parentHandle,
      reservedName: reserved.reservedName,
      collisionIndex: reserved.collisionIndex,
      singleFile: true,
      reservedAtMs,
      reservedFile: Object.freeze({
        handle: reserved.entry.handle,
        fingerprint: reserved.entry.fingerprint,
      }),
    })
  }
  return Object.freeze({
    kind: 'fsa',
    parentHandle,
    reservedName: reserved.reservedName,
    collisionIndex: reserved.collisionIndex,
    singleFile: false,
    reservedAtMs,
  })
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

export type OpenFSADirectoryResult =
  | Readonly<{ kind: 'opened'; directory: FSADirectory }>
  | Readonly<{ kind: 'conflict'; physicalPath: readonly string[] }>

export async function openFSADirectory(
  parent: FSADirectory,
  logicalPath: readonly string[],
  name: string,
  safeNames: SafeNameRegistry,
  taskId: string,
): Promise<OpenFSADirectoryResult> {
  const openOwned = async (component: string): Promise<OpenFSADirectoryResult> => {
    const candidatePath = [...parent.physicalPath, component]
    const claim = safeNames.claimDirectory(logicalPath, candidatePath)
    if (claim === 'conflict') {
      return { kind: 'conflict', physicalPath: candidatePath }
    }
    try {
      return { kind: 'opened', directory: await openDirectory(parent, component) }
    } catch (error) {
      if (claim === 'claimed') safeNames.releaseDirectoryClaim(logicalPath, candidatePath)
      throw error
    }
  }
  try {
    return await openOwned(name)
  } catch (error) {
    if (!requiresSafeComponent(error)) throw error
    const safe = await deterministicSafeComponent({ taskId, logicalPath, entryKind: 'directory' })
    const opened = await openOwned(safe)
    if (opened.kind === 'opened') {
      safeNames.recordDirectory(logicalPath, opened.directory.physicalPath)
    }
    return opened
  }
}

export async function openFSAFile(
  parent: FileSystemDirectoryHandle,
  logicalPath: readonly string[],
  leaf: string,
  safeNames: SafeNameRegistry,
  taskId: string,
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
    const name = await deterministicSafeComponent({ taskId, logicalPath, entryKind: 'file' })
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
  taskId: string,
  claimedNames: ReadonlySet<string> = EMPTY_CLAIMED_NAMES,
): Promise<ReservedFSATarget> {
  const kind = singleFile ? 'file' : 'directory'
  let skippedClaimedNames = 0
  for (let collisionIndex = 0; collisionIndex < MAX_COLLISION_ATTEMPTS; collisionIndex++) {
    let reservedName = decideCollisionName(requestedName, kind, collisionIndex)
    let entry = await tryReserveCandidate(parentHandle, reservedName, singleFile, claimedNames)
    if (entry === 'invalid-name') {
      reservedName = await deterministicSafeComponent({
        taskId,
        logicalPath: [requestedName],
        entryKind: kind,
        collisionIndex,
      })
      entry = await tryReserveCandidate(parentHandle, reservedName, singleFile, claimedNames)
      if (typeof entry !== 'string') safeNames.recordRoot([requestedName], kind, reservedName)
    }
    if (entry === 'claimed') {
      skippedClaimedNames += 1
      continue
    }
    if (typeof entry !== 'string') return { reservedName, collisionIndex, skippedClaimedNames, entry }
  }
  throw new TransferFaultError(
    outputFault(FaultScope.Job, OutputFaultCode.TargetCollision),
    'Could not allocate a unique name in the selected folder',
  )
}

async function tryReserveCandidate(
  parentHandle: FileSystemDirectoryHandle,
  candidateName: string,
  singleFile: boolean,
  claimedNames: ReadonlySet<string>,
): Promise<ReservedRootEntry | 'claimed' | 'exists' | 'invalid-name'> {
  if (claimedNames.has(foldPortableName15(candidateName))) return 'claimed'
  return reserveRootEntry(parentHandle, candidateName, singleFile)
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

/**
 * Safely removes the untouched top-level placeholder created by `openNewFSASession`
 * when startup fails before activation. Leaves externally modified files or non-empty
 * directories untouched.
 */
export async function rollbackFSATaskTarget(target: FSATargetRecord): Promise<boolean> {
  if (target.singleFile) {
    const outcome = await removeVerifiedFSALeaf(
      target.parentHandle,
      target.reservedName,
      target.reservedFile.handle,
      target.reservedFile.fingerprint,
    )
    return outcome === 'removed'
  }
  return removeDirectoryIfEmpty(target.parentHandle, target.reservedName)
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
