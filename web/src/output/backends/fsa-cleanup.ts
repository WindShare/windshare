import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import type { FSADiskFingerprint, FSATargetRecord, ReceiveFileRecord } from '../store/types'

// FAT32 truncates file timestamps downward to 2-second boundaries.
const PLACEHOLDER_MTIME_SKEW_MS = 2_000

export async function readFSADiskFingerprint(
  handle: FileSystemFileHandle,
): Promise<FSADiskFingerprint> {
  const file = await handle.getFile()
  return { size: file.size, lastModified: file.lastModified }
}

export function matchesFSADiskFingerprint(
  actual: FSADiskFingerprint,
  expected: FSADiskFingerprint | undefined,
): boolean {
  return actual.size === expected?.size && actual.lastModified === expected.lastModified
}

/**
 * Removes only task-owned unfinished files verified by handle containment and disk
 * fingerprint, then prunes directories non-recursively so foreign or completed files
 * are never deleted. Never uses `{ recursive: true }` on user-owned folders.
 */
export async function cleanupFSATaskEntries(
  taskId: string,
  target: FSATargetRecord,
  files: readonly ReceiveFileRecord[],
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  const rootDir = target.singleFile
    ? target.parentHandle
    : await target.parentHandle.getDirectoryHandle(target.reservedName).catch(() => undefined)
  if (rootDir === undefined) return

  const hasCompleted = files.some(file => file.status === 'completed')
  const candidateDirs = new Map<string, readonly string[]>()
  let removedFiles = 0
  let skippedFiles = 0

  for (const file of files) {
    if (file.status === 'completed') continue
    const outcome = await removeVerifiedUnfinishedFile(rootDir, target, file, candidateDirs)
    if (outcome === 'removed') removedFiles += 1
    else if (outcome === 'skipped') skippedFiles += 1
  }

  let removedRoot = false
  if (!target.singleFile) {
    await pruneEmptyDirectoryPaths(rootDir, candidateDirs)
    if (!hasCompleted) {
      removedRoot = await removeDirectoryIfEmpty(target.parentHandle, target.reservedName)
    }
  }

  emitOutputTrace(diagnostics?.trace, 'cleanup', {
    backend: 'file_system_access',
    task_id: taskId,
    transition: 'completed',
    removed_files: removedFiles,
    skipped_unverified_files: skippedFiles,
    removed_root: removedRoot,
  })
}

/**
 * Removes a newly created 0-byte placeholder left when an uncheckpointed write is
 * aborted, refusing to touch non-empty or older pre-existing files in a reopened folder.
 */
export async function removeUncommittedEmptyPlaceholder(
  parentDir: FileSystemDirectoryHandle,
  physicalName: string,
  expectedHandle: FileSystemFileHandle,
  openedAtMs: number,
): Promise<void> {
  const current = await parentDir.getFileHandle(physicalName).catch(() => undefined)
  if (current === undefined || !await isSameHandle(current, expectedHandle)) return
  const file = await current.getFile().catch(() => undefined)
  if (file === undefined || file.size !== 0 || file.lastModified < openedAtMs - PLACEHOLDER_MTIME_SKEW_MS) {
    return
  }
  await parentDir.removeEntry(physicalName).catch(() => undefined)
}

/**
 * Removes empty subdirectories created during a stopped session, deepest first,
 * without touching the result root or any non-empty directory.
 */
export async function pruneEmptySubdirectories(
  rootDir: FileSystemDirectoryHandle,
  directories: readonly FileSystemDirectoryHandle[],
): Promise<void> {
  const candidatePaths = new Map<string, readonly string[]>()
  for (const dir of directories) {
    const relativePath = await resolveRelativePath(rootDir, dir)
    if (relativePath === null || relativePath.length === 0) continue
    collectDirectoryPrefixes(candidatePaths, relativePath)
  }
  await pruneEmptyDirectoryPaths(rootDir, candidatePaths)
}

type FileCleanupOutcome = 'removed' | 'missing' | 'skipped'

async function removeVerifiedUnfinishedFile(
  rootDir: FileSystemDirectoryHandle,
  target: FSATargetRecord,
  file: ReceiveFileRecord,
  candidateDirs: Map<string, readonly string[]>,
): Promise<FileCleanupOutcome> {
  if (file.handle === undefined || file.fsaDiskFingerprint === undefined) return 'skipped'
  const relativePath = await resolveRelativePath(rootDir, file.handle)
  if (relativePath === null || relativePath.length === 0) return 'skipped'
  if (target.singleFile && (relativePath.length !== 1 || relativePath[0] !== target.reservedName)) {
    return 'skipped'
  }

  const parentPath = relativePath.slice(0, -1)
  const parentDir = await resolveDirectoryChain(rootDir, parentPath)
  if (parentDir === undefined) return 'missing'

  const leafName = relativePath.at(-1)!
  const outcome = await verifyAndRemoveLeaf(parentDir, leafName, file.handle, file.fsaDiskFingerprint)
  if (outcome === 'removed' && parentPath.length > 0) {
    collectDirectoryPrefixes(candidateDirs, parentPath)
  }
  return outcome
}

async function verifyAndRemoveLeaf(
  parentDir: FileSystemDirectoryHandle,
  leafName: string,
  expectedHandle: FileSystemFileHandle,
  expectedFingerprint: FSADiskFingerprint,
): Promise<FileCleanupOutcome> {
  let currentHandle: FileSystemFileHandle
  try {
    currentHandle = await parentDir.getFileHandle(leafName)
  } catch (error) {
    return error instanceof DOMException && error.name === 'NotFoundError' ? 'missing' : 'skipped'
  }

  if (!await isSameHandle(currentHandle, expectedHandle)) return 'skipped'
  // Chromium's native `isSameEntry` compares paths rather than OS inodes, so checking
  // both size and lastModified prevents deleting a same-name user replacement.
  const current = await readFSADiskFingerprint(currentHandle).catch(() => undefined)
  if (current === undefined || !matchesFSADiskFingerprint(current, expectedFingerprint)) {
    return 'skipped'
  }

  try {
    await parentDir.removeEntry(leafName)
    return 'removed'
  } catch {
    return 'skipped'
  }
}

function collectDirectoryPrefixes(
  target: Map<string, readonly string[]>,
  path: readonly string[],
): void {
  for (let depth = 1; depth <= path.length; depth += 1) {
    const prefix = path.slice(0, depth)
    target.set(JSON.stringify(prefix), prefix)
  }
}

async function pruneEmptyDirectoryPaths(
  rootDir: FileSystemDirectoryHandle,
  candidateDirs: ReadonlyMap<string, readonly string[]>,
): Promise<void> {
  const ordered = [...candidateDirs.values()].sort((left, right) => right.length - left.length)
  for (const dirPath of ordered) {
    const parent = await resolveDirectoryChain(rootDir, dirPath.slice(0, -1))
    if (parent === undefined) continue
    await removeDirectoryIfEmpty(parent, dirPath.at(-1)!)
  }
}

async function removeDirectoryIfEmpty(
  parentDir: FileSystemDirectoryHandle,
  name: string,
): Promise<boolean> {
  try {
    // Verify the entry is still a directory so a same-name regular file is never deleted,
    // then rely on non-recursive removeEntry to reject non-empty directories atomically.
    await parentDir.getDirectoryHandle(name)
    await parentDir.removeEntry(name)
    return true
  } catch {
    return false
  }
}

async function resolveDirectoryChain(
  rootDir: FileSystemDirectoryHandle,
  segments: readonly string[],
): Promise<FileSystemDirectoryHandle | undefined> {
  let current = rootDir
  for (const segment of segments) {
    const next = await current.getDirectoryHandle(segment).catch(() => undefined)
    if (next === undefined) return undefined
    current = next
  }
  return current
}

async function resolveRelativePath(
  rootDir: FileSystemDirectoryHandle,
  descendant: FileSystemHandle,
): Promise<readonly string[] | null> {
  if (typeof rootDir.resolve !== 'function') return null
  try {
    return await rootDir.resolve(descendant)
  } catch {
    return null
  }
}

async function isSameHandle(
  left: FileSystemHandle,
  right: FileSystemHandle,
): Promise<boolean> {
  if (left === right) return true
  if (typeof left.isSameEntry !== 'function') return false
  try {
    return await left.isSameEntry(right)
  } catch {
    return false
  }
}
