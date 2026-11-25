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
import { SafeNameRegistry, decideCollisionName, deterministicSafeCandidates } from './safe-name'

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

type PermissionHandle = FileSystemDirectoryHandle & {
  queryPermission?: (desc: { mode: 'read' | 'readwrite' }) => Promise<'granted' | 'denied' | 'prompt'>
  requestPermission?: (desc: { mode: 'read' | 'readwrite' }) => Promise<'granted' | 'denied' | 'prompt'>
}

export async function ensureFSAPermission(
  handle: FileSystemDirectoryHandle,
  allowPrompt: boolean,
  signal?: AbortSignal,
): Promise<'granted' | 'prompt' | 'denied'> {
  signal?.throwIfAborted()
  const permHandle = handle as PermissionHandle
  if (typeof permHandle.queryPermission !== 'function') return 'granted'
  const current = await permHandle.queryPermission({ mode: 'readwrite' })
  signal?.throwIfAborted()
  if (current === 'granted') return 'granted'
  if (!allowPrompt || typeof permHandle.requestPermission !== 'function') return current
  try {
    return await permHandle.requestPermission({ mode: 'readwrite' })
  } catch (error) {
    // requestPermission() rejects with SecurityError when transient user activation
    // has expired; leave permission ungranted so callers surface PermissionDenied.
    if (error instanceof DOMException && error.name === 'SecurityError') return current
    throw error
  }
}

function isNameOrPolicyRefusal(error: unknown): boolean {
  return error instanceof TypeError || (error instanceof DOMException && error.name === 'SecurityError')
}

/**
 * Classifies browser-side refusals at `getFileHandle` / `getDirectoryHandle`
 * that should fall back to a deterministic safe component name: invalid
 * component syntax or blocked extensions (`TypeError`), case-insensitive
 * opposite-kind collisions inside the task root (`TypeMismatchError`), and
 * path-scoped write-policy blocks such as `.git/hooks` or `.git/config`
 * (`SecurityError`). Missing folder permission throws `NotAllowedError` and
 * must propagate untouched.
 */
export function requiresSafeComponent(error: unknown): boolean {
  return isNameOrPolicyRefusal(error) || (error instanceof DOMException && error.name === 'TypeMismatchError')
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
    const [safe] = await deterministicSafeCandidates({ taskId, logicalPath, entryKind: 'directory' })
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
  try {
    return await openOrCreateFSAFile(parent, leaf)
  } catch (error) {
    if (!requiresSafeComponent(error)) throw error
    return openSafeFSAFile(parent, logicalPath, safeNames, taskId, error)
  }
}

async function openOrCreateFSAFile(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<{ handle: FileSystemFileHandle; created: boolean }> {
  try {
    return { handle: await parent.getFileHandle(name), created: false }
  } catch (error) {
    if (!isMissingFSAEntry(error)) throw error
  }
  return { handle: await parent.getFileHandle(name, { create: true }), created: true }
}

async function openSafeFSAFile(
  parent: FileSystemDirectoryHandle,
  logicalPath: readonly string[],
  safeNames: SafeNameRegistry,
  taskId: string,
  initialError: unknown,
): Promise<{ handle: FileSystemFileHandle; created: boolean }> {
  const candidates = await deterministicSafeCandidates({ taskId, logicalPath, entryKind: 'file' })
  let lastError: unknown = initialError
  for (const candidate of candidates) {
    try {
      const entry = await openOrCreateFSAFile(parent, candidate)
      safeNames.record(logicalPath, 'file', candidate)
      return entry
    } catch (error) {
      if (!requiresSafeComponent(error)) throw error
      lastError = error
    }
  }
  throw lastError
}

type RejectedCandidate = Readonly<{ kind: 'rejected'; error: unknown }>
type ProbeResult = 'missing' | 'exists' | RejectedCandidate

function isRejectedCandidate(value: unknown): value is RejectedCandidate {
  return typeof value === 'object' && value !== null && 'kind' in value && value.kind === 'rejected'
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
    if (isRejectedCandidate(entry)) {
      const safe = await reserveSafeTargetCandidate(
        parentHandle, requestedName, singleFile, taskId, collisionIndex, claimedNames, entry.error,
      )
      reservedName = safe.reservedName
      entry = safe.entry
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

async function reserveSafeTargetCandidate(
  parentHandle: FileSystemDirectoryHandle,
  requestedName: string,
  singleFile: boolean,
  taskId: string,
  collisionIndex: number,
  claimedNames: ReadonlySet<string>,
  initialError: unknown,
): Promise<Readonly<{
  reservedName: string
  entry: ReservedRootEntry | 'claimed' | 'exists'
}>> {
  const kind = singleFile ? 'file' : 'directory'
  const candidates = await deterministicSafeCandidates({
    taskId,
    logicalPath: [requestedName],
    entryKind: kind,
    collisionIndex,
  })
  let lastError: unknown = initialError
  for (const candidate of candidates) {
    const entry = await tryReserveCandidate(parentHandle, candidate, singleFile, claimedNames)
    // Only another name/policy refusal justifies dropping the extension; 'exists' and 'claimed'
    // show the browser accepted this extension and the outer loop should increment collisionIndex.
    if (!isRejectedCandidate(entry)) return { reservedName: candidate, entry }
    lastError = entry.error
  }
  // Every deterministic portable candidate (ending with an extensionless `.windshare-<hash>` name)
  // was rejected; advancing collisionIndex cannot resolve a directory-wide or policy refusal.
  throw lastError
}

async function tryReserveCandidate(
  parentHandle: FileSystemDirectoryHandle,
  candidateName: string,
  singleFile: boolean,
  claimedNames: ReadonlySet<string>,
): Promise<ReservedRootEntry | 'claimed' | 'exists' | RejectedCandidate> {
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
): Promise<ReservedRootEntry | 'exists' | RejectedCandidate> {
  const state = await probeEntry(parent, name)
  if (state !== 'missing') return state
  let handle: FileSystemFileHandle
  try {
    if (!singleFile) {
      return { kind: 'directory', handle: await parent.getDirectoryHandle(name, { create: true }) }
    }
    handle = await parent.getFileHandle(name, { create: true })
  } catch (error) {
    if (isNameOrPolicyRefusal(error)) return { kind: 'rejected', error }
    if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'exists'
    throw error
  }
  const fingerprint = await readFSADiskFingerprint(handle)
  // An external writer can populate the same path after entryExists; never adopt non-empty bytes.
  if (fingerprint.size !== 0) return 'exists'
  return { kind: 'file', handle, fingerprint }
}

export async function entryExists(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<'missing' | 'exists' | 'invalid-name'> {
  const state = await probeEntry(parent, name)
  return isRejectedCandidate(state) ? 'invalid-name' : state
}

async function probeEntry(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<ProbeResult> {
  const fileProbe = await probeHandle(() => parent.getFileHandle(name))
  if (fileProbe !== 'missing') return fileProbe
  return probeHandle(() => parent.getDirectoryHandle(name))
}

async function probeHandle(open: () => Promise<unknown>): Promise<ProbeResult> {
  try {
    await open()
    return 'exists'
  } catch (error) {
    if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'exists'
    if (isNameOrPolicyRefusal(error)) return { kind: 'rejected', error }
    if (isMissingFSAEntry(error)) return 'missing'
    throw error
  }
}
