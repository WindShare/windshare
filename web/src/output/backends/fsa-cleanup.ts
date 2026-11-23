import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import type { ReceiveStore } from '../store/database'
import type { FSADiskFingerprint, FSATargetRecord, ReceiveFileRecord } from '../store/types'

// FAT32 truncates file timestamps downward to 2-second boundaries.
const PLACEHOLDER_MTIME_SKEW_MS = 2_000

export function isUncommittedEmptyPlaceholder(
  fingerprint: FSADiskFingerprint,
  earliestMs: number,
  latestMs: number,
): boolean {
  return fingerprint.size === 0 &&
    fingerprint.lastModified >= earliestMs - PLACEHOLDER_MTIME_SKEW_MS &&
    fingerprint.lastModified <= latestMs
}

export async function readFSADiskFingerprint(handle: FileSystemFileHandle): Promise<FSADiskFingerprint> {
  const file = await handle.getFile()
  return { size: file.size, lastModified: file.lastModified }
}

export function matchesFSADiskFingerprint(actual: FSADiskFingerprint, expected?: FSADiskFingerprint): boolean {
  return actual.size === expected?.size && actual.lastModified === expected.lastModified
}

export function isMissingFSAEntry(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'NotFoundError'
}

function entryFailure(error: unknown): 'missing' | 'changed' {
  if (isMissingFSAEntry(error)) return 'missing'
  if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'changed'
  throw error
}

export async function sameFSAEntry(left: FileSystemHandle, right: FileSystemHandle): Promise<boolean> {
  return left === right || (typeof left.isSameEntry === 'function' && await left.isSameEntry(right))
}

type CleanupOutcome = 'removed' | 'missing' | 'changed'

export async function readFSALeafFingerprint(
  parent: FileSystemDirectoryHandle, name: string, expected: FileSystemFileHandle,
): Promise<FSADiskFingerprint | undefined> {
  const current = await parent.getFileHandle(name)
  return await sameFSAEntry(current, expected) ? readFSADiskFingerprint(current) : undefined
}

export async function inspectFSALeaf(
  parent: FileSystemDirectoryHandle, name: string, expected: FileSystemFileHandle, fingerprint: FSADiskFingerprint,
): Promise<'matching' | 'missing' | 'changed'> {
  try {
    // Native handles can compare equal after a same-path replacement.
    const current = await readFSALeafFingerprint(parent, name, expected)
    return current !== undefined && matchesFSADiskFingerprint(current, fingerprint) ? 'matching' : 'changed'
  } catch (error) {
    return entryFailure(error)
  }
}

export async function removeVerifiedFSALeaf(
  parent: FileSystemDirectoryHandle, name: string, expected: FileSystemFileHandle,
  fingerprint: FSADiskFingerprint, beginRemoval?: () => void,
): Promise<CleanupOutcome> {
  try {
    const state = await inspectFSALeaf(parent, name, expected, fingerprint)
    if (state !== 'matching') return state
    beginRemoval?.()
    await parent.removeEntry(name)
    return 'removed'
  } catch (error) {
    return entryFailure(error)
  }
}

export async function removeUncommittedEmptyPlaceholder(
  parent: FileSystemDirectoryHandle, name: string, expected: FileSystemFileHandle, openedAtMs: number, acquiredAtMs: number,
): Promise<CleanupOutcome> {
  const current = await parent.getFileHandle(name).catch(entryFailure)
  if (typeof current === 'string') return current
  if (!await sameFSAEntry(current, expected)) return 'changed'
  const file = await readFSADiskFingerprint(current)
  if (expected !== current && !matchesFSADiskFingerprint(await readFSADiskFingerprint(expected), file)) return 'changed'
  if (!isUncommittedEmptyPlaceholder(file, openedAtMs, acquiredAtMs)) return 'changed'
  return removeVerifiedFSALeaf(parent, name, expected, file)
}

interface CleanupOptions {
  store: Pick<ReceiveStore, 'deleteFiles'>
  diagnostics: OutputDiagnosticsPorts | undefined
  directories?: readonly FileSystemDirectoryHandle[]
  beginRemoval?: () => void
}

/** Only confirmed removals retire checkpoint rows. An I/O error leaves the rest available for another discard. */
export async function cleanupFSATaskEntries(
  taskId: string, target: FSATargetRecord, files: readonly ReceiveFileRecord[], options: CleanupOptions,
): Promise<void> {
  const root = target.singleFile ? target.parentHandle :
    await target.parentHandle.getDirectoryHandle(target.reservedName).catch(entryFailure)
  const unfinished = files.filter(file => file.status !== 'completed')
  const directories = new Map<string, readonly string[]>()
  const { retired, removed, changed, failures } = await cleanupUnfinishedFiles(root, target, unfinished, directories, options)
  let removedRoot = false
  if (!target.singleFile && typeof root !== 'string') {
    try {
      removedRoot = await pruneTaskDirectories(root, target, directories, files.length === unfinished.length, options)
    } catch (error) {
      failures.push(error)
    }
  }
  if (retired.length > 0) {
    options.beginRemoval?.()
    await options.store.deleteFiles(retired)
  }
  emitOutputTrace(options.diagnostics?.trace, 'cleanup', {
    backend: 'file_system_access', task_id: taskId, transition: failures.length === 0 ? 'completed' : 'failed',
    removed_files: removed, skipped_unverified_files: changed, removed_root: removedRoot,
  })
  if (failures.length > 0) throw failures[0]
}

async function cleanupUnfinishedFiles(
  root: FileSystemDirectoryHandle | 'missing' | 'changed', target: FSATargetRecord, unfinished: readonly ReceiveFileRecord[],
  directories: Map<string, readonly string[]>, options: CleanupOptions,
) {
  const retired: string[] = []
  let removed = 0, changed = 0
  const failures: unknown[] = []

  for (const file of unfinished) {
    try {
      const outcome = typeof root === 'string' ? root :
        await removeUnfinishedFile(root, target, file, directories, options.beginRemoval)
      if (outcome === 'changed') changed++
      else retired.push(file.id)
      if (outcome === 'removed') removed++
    } catch (error) {
      failures.push(error)
    }
  }

  return { retired, removed, changed, failures }
}

async function pruneTaskDirectories(
  root: FileSystemDirectoryHandle, target: FSATargetRecord, directories: Map<string, readonly string[]>,
  removeRoot: boolean, options: CleanupOptions,
): Promise<boolean> {
  for (const handle of options.directories ?? []) {
    const path = await root.resolve(handle)
    if (path !== null) collectDirectoryPrefixes(directories, path)
  }
  await pruneEmptyDirectoryPaths(root, directories, options.beginRemoval)
  return removeRoot && await removeDirectoryIfEmpty(target.parentHandle, target.reservedName, options.beginRemoval)
}

async function removeUnfinishedFile(
  root: FileSystemDirectoryHandle, target: FSATargetRecord, file: ReceiveFileRecord,
  directories: Map<string, readonly string[]>, beginRemoval?: () => void,
): Promise<CleanupOutcome> {
  if (file.handle === undefined) return 'changed'
  // A pending close proves ownership only of the version captured *before* publication.
  const fingerprint = file.fsaPendingPublish?.fingerprint ?? file.fsaDiskFingerprint
  if (fingerprint === undefined) return 'changed'
  const path = file.fsaPath ?? await root.resolve(file.handle)
  if (!path?.length || (target.singleFile && (path.length !== 1 || path[0] !== target.reservedName))) return 'changed'
  const parentPath = path.slice(0, -1)
  const parent = await resolveDirectoryChain(root, parentPath)
  if (typeof parent === 'string') return parent
  const outcome = await removeVerifiedFSALeaf(parent, path[path.length - 1]!, file.handle, fingerprint, beginRemoval)
  if (outcome !== 'changed') collectDirectoryPrefixes(directories, parentPath)
  return outcome
}

function collectDirectoryPrefixes(directories: Map<string, readonly string[]>, path: readonly string[]): void {
  for (let depth = 1; depth <= path.length; depth++) {
    const prefix = path.slice(0, depth)
    directories.set(JSON.stringify(prefix), prefix)
  }
}

async function pruneEmptyDirectoryPaths(
  root: FileSystemDirectoryHandle, directories: ReadonlyMap<string, readonly string[]>, beginRemoval?: () => void,
): Promise<void> {
  const ordered = [...directories.values()].sort((left, right) => right.length - left.length)
  for (const path of ordered) {
    const parent = await resolveDirectoryChain(root, path.slice(0, -1))
    if (typeof parent !== 'string') await removeDirectoryIfEmpty(parent, path[path.length - 1]!, beginRemoval)
  }
}

async function removeDirectoryIfEmpty(parent: FileSystemDirectoryHandle, name: string, beginRemoval?: () => void): Promise<boolean> {
  try {
    await parent.getDirectoryHandle(name)
    beginRemoval?.()
    // A non-recursive remove lets the filesystem protect completed files and unrelated descendants.
    await parent.removeEntry(name)
    return true
  } catch (error) {
    if (error instanceof DOMException && error.name === 'InvalidModificationError') return false
    entryFailure(error)
    return false
  }
}

async function resolveDirectoryChain(
  root: FileSystemDirectoryHandle, segments: readonly string[],
): Promise<FileSystemDirectoryHandle | 'missing' | 'changed'> {
  let current = root
  for (const segment of segments) {
    const next = await current.getDirectoryHandle(segment).catch(entryFailure)
    if (typeof next === 'string') return next
    current = next
  }
  return current
}
