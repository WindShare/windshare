import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import { maxRangeEnd, type FSATargetRecord, type ReceiveFileRecord } from '../store/types'

type IterableDirectoryHandle = FileSystemDirectoryHandle & {
  keys?: () => AsyncIterableIterator<string>
  values?: () => AsyncIterableIterator<FileSystemHandle>
}

export interface PhysicalFileStat {
  readonly physicalSize: bigint
  readonly physicalLastModified: number
}

export async function readPhysicalFileStat(
  handle: FileSystemFileHandle,
): Promise<PhysicalFileStat | undefined> {
  try {
    const file = await handle.getFile()
    return Object.freeze({
      physicalSize: BigInt(file.size),
      physicalLastModified: file.lastModified,
    })
  } catch {
    return undefined
  }
}

/**
 * Removes only task-owned unfinished files verified by handle identity and physical
 * stat, then prunes directories non-recursively so foreign or completed files are
 * never deleted. Never uses `{ recursive: true }` on user-owned folders.
 */
export async function cleanupFSATaskEntries(
  taskId: string,
  target: FSATargetRecord,
  files: readonly ReceiveFileRecord[],
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  if (target.singleFile) {
    await cleanupSingleFileTask(taskId, target, files, diagnostics)
    return
  }
  await cleanupDirectoryTask(taskId, target, files, diagnostics)
}

/**
 * Removes a newly created 0-byte placeholder file when an uncheckpointed write is
 * aborted before any bytes are committed. Refuses to delete non-empty or foreign files.
 */
export async function removeFreshEmptyFile(
  parentDir: FileSystemDirectoryHandle,
  physicalName: string,
  expectedHandle: FileSystemFileHandle,
): Promise<void> {
  const current = await parentDir.getFileHandle(physicalName).catch(() => undefined)
  if (current === undefined) return
  if (!await isSameHandle(current, expectedHandle)) return
  const file = await current.getFile().catch(() => undefined)
  if (file === undefined || file.size !== 0) return
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

async function cleanupSingleFileTask(
  taskId: string,
  target: FSATargetRecord,
  files: readonly ReceiveFileRecord[],
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  if (files.some(file => file.status === 'completed')) {
    emitCleanupTrace(diagnostics, taskId, 'completed', 0, 0, false)
    return
  }
  let removedFiles = 0
  let skippedFiles = 0
  for (const file of files) {
    const outcome = await removeVerifiedSingleFile(target.parentHandle, target.reservedName, file)
    if (outcome === 'removed') removedFiles += 1
    else if (outcome === 'skipped') skippedFiles += 1
  }
  emitCleanupTrace(diagnostics, taskId, 'completed', removedFiles, skippedFiles, false)
}

async function cleanupDirectoryTask(
  taskId: string,
  target: FSATargetRecord,
  files: readonly ReceiveFileRecord[],
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  const rootDir = await target.parentHandle.getDirectoryHandle(target.reservedName).catch(() => undefined)
  if (rootDir === undefined) return

  if (target.rootHandle !== undefined && !await isSameHandle(rootDir, target.rootHandle)) {
    emitOutputTrace(diagnostics?.trace, 'cleanup', {
      backend: 'file_system_access',
      task_id: taskId,
      transition: 'skipped_unverified_root',
    })
    return
  }

  const candidateDirs = new Map<string, readonly string[]>()
  let removedFiles = 0
  let skippedFiles = 0
  for (const file of files) {
    if (file.status === 'completed') continue
    const outcome = await removeVerifiedTreeFile(rootDir, file, candidateDirs)
    if (outcome === 'removed') removedFiles += 1
    else if (outcome === 'skipped') skippedFiles += 1
  }

  await pruneEmptyDirectoryPaths(rootDir, candidateDirs)

  let removedRoot = false
  if (!files.some(file => file.status === 'completed')) {
    removedRoot = await removeEmptyDirectory(target.parentHandle, target.reservedName, rootDir)
  }
  emitCleanupTrace(diagnostics, taskId, 'completed', removedFiles, skippedFiles, removedRoot)
}

type FileCleanupOutcome = 'removed' | 'missing' | 'skipped'

async function removeVerifiedSingleFile(
  parentHandle: FileSystemDirectoryHandle,
  reservedName: string,
  file: ReceiveFileRecord,
): Promise<FileCleanupOutcome> {
  if (file.handle === undefined) return 'skipped'
  const relativePath = await resolveRelativePath(parentHandle, file.handle)
  if (relativePath === null || relativePath.length !== 1 || relativePath[0] !== reservedName) {
    return 'skipped'
  }
  return verifyAndRemoveLeaf(parentHandle, reservedName, file)
}

async function removeVerifiedTreeFile(
  rootDir: FileSystemDirectoryHandle,
  file: ReceiveFileRecord,
  candidateDirs: Map<string, readonly string[]>,
): Promise<FileCleanupOutcome> {
  if (file.handle === undefined) return 'skipped'
  const relativePath = await resolveRelativePath(rootDir, file.handle)
  if (relativePath === null || relativePath.length === 0) return 'skipped'

  const parentPath = relativePath.slice(0, -1)
  const parentDir = await resolveDirectoryChain(rootDir, parentPath)
  if (parentDir === undefined) return 'missing'

  const outcome = await verifyAndRemoveLeaf(parentDir, relativePath.at(-1)!, file)
  if (outcome === 'removed' && parentPath.length > 0) {
    collectDirectoryPrefixes(candidateDirs, parentPath)
  }
  return outcome
}

async function verifyAndRemoveLeaf(
  parentDir: FileSystemDirectoryHandle,
  leafName: string,
  file: ReceiveFileRecord,
): Promise<FileCleanupOutcome> {
  const probe = await probeFileHandle(parentDir, leafName)
  if (probe.kind === 'missing') return 'missing'
  if (probe.kind === 'mismatch') return 'skipped'
  if (file.handle === undefined || !await isSameHandle(probe.handle, file.handle)) return 'skipped'
  if (!await matchesRecordedPhysicalFile(probe.handle, file)) return 'skipped'

  try {
    await parentDir.removeEntry(leafName)
    return 'removed'
  } catch {
    return 'skipped'
  }
}

async function probeFileHandle(
  parentDir: FileSystemDirectoryHandle,
  name: string,
): Promise<
  | Readonly<{ kind: 'found'; handle: FileSystemFileHandle }>
  | Readonly<{ kind: 'missing' }>
  | Readonly<{ kind: 'mismatch' }>
> {
  try {
    return { kind: 'found', handle: await parentDir.getFileHandle(name) }
  } catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') {
      return { kind: 'missing' }
    }
    return { kind: 'mismatch' }
  }
}

/**
 * Chromium's native `isSameEntry` compares handle URLs (paths) rather than OS inodes,
 * so checking the last recorded size and mtime prevents deleting a foreign file that
 * replaced the unfinished download at the exact same path.
 */
async function matchesRecordedPhysicalFile(
  handle: FileSystemFileHandle,
  record: ReceiveFileRecord,
): Promise<boolean> {
  const current = await readPhysicalFileStat(handle)
  if (current === undefined) return false
  if (record.physicalSize !== undefined && current.physicalSize !== record.physicalSize) {
    return false
  }
  if (
    record.physicalLastModified !== undefined &&
    current.physicalLastModified !== record.physicalLastModified
  ) {
    return false
  }
  if (record.physicalSize === undefined && current.physicalSize !== maxRangeEnd(record.completedRanges)) {
    return false
  }
  return true
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
    const leafName = dirPath.at(-1)!
    const child = await parent.getDirectoryHandle(leafName).catch(() => undefined)
    if (child === undefined) continue
    await removeEmptyDirectory(parent, leafName, child)
  }
}

async function removeEmptyDirectory(
  parentDir: FileSystemDirectoryHandle,
  name: string,
  dir: FileSystemDirectoryHandle,
): Promise<boolean> {
  if (!await isDirectoryEmpty(dir)) return false
  try {
    // Non-recursive removeEntry relies on the OS to reject non-empty directories atomically.
    await parentDir.removeEntry(name)
    return true
  } catch {
    return false
  }
}

async function isDirectoryEmpty(dir: FileSystemDirectoryHandle): Promise<boolean> {
  const iterable = dir as IterableDirectoryHandle
  try {
    if (typeof iterable.keys === 'function') {
      return await isAsyncIteratorEmpty(iterable.keys())
    }
    if (typeof iterable.values === 'function') {
      return await isAsyncIteratorEmpty(iterable.values())
    }
    return true
  } catch {
    return false
  }
}

async function isAsyncIteratorEmpty(iterator: AsyncIterableIterator<unknown>): Promise<boolean> {
  try {
    const first = await iterator.next()
    return Boolean(first.done)
  } finally {
    await iterator.return?.().catch(() => undefined)
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

function emitCleanupTrace(
  diagnostics: OutputDiagnosticsPorts | undefined,
  taskId: string,
  transition: string,
  removedFiles: number,
  skippedUnverifiedFiles: number,
  removedRoot: boolean,
): void {
  emitOutputTrace(diagnostics?.trace, 'cleanup', {
    backend: 'file_system_access',
    task_id: taskId,
    transition,
    removed_files: removedFiles,
    skipped_unverified_files: skippedUnverifiedFiles,
    removed_root: removedRoot,
  })
}
