import type { CatalogClient } from '../catalog/client'
import { snapshotCatalogPath } from '../catalog/path-policy'
import type { CatalogEntry, ShareDescriptor } from '../catalog/records'
import type { FrozenSelectionPolicy } from '../catalog/selection'

export interface DiscoveredDirectory {
  readonly kind: 'directory'
  readonly entry: Extract<CatalogEntry, { kind: 'directory' }>
  readonly sourcePath: readonly string[]
  readonly relativePath: readonly string[]
  readonly selected: boolean
}

export interface DiscoveredFile {
  readonly kind: 'file'
  readonly entry: Extract<CatalogEntry, { kind: 'file' }>
  readonly sourcePath: readonly string[]
  readonly relativePath: readonly string[]
}

export type DiscoveredItem = DiscoveredDirectory | DiscoveredFile

export type SelectionShape =
  | Readonly<{ kind: 'empty' }>
  | Readonly<{
      kind: 'single-file'
      file: Extract<CatalogEntry, { kind: 'file' }>
      sourcePath: readonly string[]
      exactBytes: bigint
    }>
  | Readonly<{
      kind: 'tree'
      suggestedName: string
      knownFileCount: number
      knownBytes: bigint
      complete: boolean
    }>

export interface DiscoveryCallbacks {
  readonly onDirectoryFailed?: (directoryId: string, error: unknown) => void
  readonly onOmittedEntries?: (count: bigint) => void
  readonly onFileDiscovered?: (file: DiscoveredFile) => void
}

const DEFAULT_ARCHIVE_NAME = 'windshare-download'
const SHAPE_PROBE_DIRECTORY_LIMIT = 4

type CommittedDir = Awaited<ReturnType<CatalogClient['loadDirectory']>>

interface ProbeFrame {
  readonly id: Uint8Array
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly committed?: CommittedDir
}

interface ProbeState {
  singleFile: { file: Extract<CatalogEntry, { kind: 'file' }>; sourcePath: readonly string[] } | undefined
  selectedFiles: number
  selectedDirectories: number
  knownBytes: bigint
  firstRootName: string | undefined
  complete: boolean
}

/**
 * Inspects just enough of the authenticated catalog to classify the user's selection
 * (empty / single file / multi-item tree) without forcing a full-tree scan upfront.
 */
export async function inspectSelectionShape(
  descriptor: ShareDescriptor,
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  signal: AbortSignal,
): Promise<SelectionShape> {
  const rootCommitted = await catalog.loadDirectory(descriptor.syntheticRoot, { signal })
  const rootAncestry = [descriptor.syntheticRootId]
  const rootEntries = await readFirstEntries(catalog, rootCommitted, signal, 3)

  // Fast path: sole root file selected.
  if (rootCommitted.entryCount === 1 && rootEntries[0]?.kind === 'file') {
    const file = rootEntries[0]
    if (!selection.selected(file, rootAncestry)) return Object.freeze({ kind: 'empty' })
    return Object.freeze({
      kind: 'single-file',
      file,
      sourcePath: Object.freeze([file.name]),
      exactBytes: file.expectedSize,
    })
  }

  return probeSelectionSubtree(descriptor, catalog, selection, rootCommitted, signal)
}

async function readFirstEntries(
  catalog: CatalogClient,
  committed: CommittedDir,
  signal: AbortSignal,
  limit: number,
): Promise<CatalogEntry[]> {
  const entries: CatalogEntry[] = []
  for (let pageIndex = 0; pageIndex < committed.pageCount; pageIndex++) {
    signal.throwIfAborted()
    const page = await catalog.page(committed, pageIndex, signal)
    entries.push(...page.entries)
    if (entries.length >= limit) break
  }
  return entries
}

async function probeSelectionSubtree(
  descriptor: ShareDescriptor,
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  rootCommitted: CommittedDir,
  signal: AbortSignal,
): Promise<SelectionShape> {
  const state: ProbeState = {
    singleFile: undefined,
    selectedFiles: 0,
    selectedDirectories: 0,
    knownBytes: 0n,
    firstRootName: undefined,
    complete: true,
  }
  const stack: ProbeFrame[] = [{
    id: descriptor.syntheticRoot,
    idText: descriptor.syntheticRootId,
    path: [],
    ancestry: [descriptor.syntheticRootId],
    committed: rootCommitted,
  }]

  let scannedDirectories = 0
  while (stack.length > 0 && state.complete) {
    signal.throwIfAborted()
    if (scannedDirectories >= SHAPE_PROBE_DIRECTORY_LIMIT) {
      state.complete = false
      break
    }
    const dir = stack.pop()!
    scannedDirectories += 1
    const committed = dir.committed ?? await catalog.loadDirectory(dir.id, { signal })
    await probeDirectoryPages(catalog, selection, dir, committed, stack, state, signal)
  }

  return buildProbeShape(state)
}

async function probeDirectoryPages(
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  dir: ProbeFrame,
  committed: CommittedDir,
  stack: ProbeFrame[],
  state: ProbeState,
  signal: AbortSignal,
): Promise<void> {
  for (let pageIndex = 0; pageIndex < committed.pageCount; pageIndex++) {
    signal.throwIfAborted()
    const page = await catalog.page(committed, pageIndex, signal)
    for (const entry of page.entries) {
      recordProbedEntry(selection, dir, entry, stack, state)
    }
    if (state.selectedDirectories > 0 && state.selectedFiles > 1) {
      state.complete = false
      return
    }
  }
}

function recordProbedEntry(
  selection: FrozenSelectionPolicy,
  dir: ProbeFrame,
  entry: CatalogEntry,
  stack: ProbeFrame[],
  state: ProbeState,
): void {
  const entryPath = snapshotCatalogPath([...dir.path, entry.name])
  const selected = selection.selected(entry, dir.ancestry)
  if (entry.kind === 'file') {
    if (!selected) return
    state.selectedFiles += 1
    state.knownBytes += entry.expectedSize
    state.firstRootName ??= entryPath[0]
    state.singleFile = state.selectedFiles === 1 && state.selectedDirectories === 0
      ? { file: entry, sourcePath: entryPath }
      : undefined
    return
  }
  if (selected) {
    state.selectedDirectories += 1
    state.singleFile = undefined
    state.firstRootName ??= entry.name
  }
  if (selection.shouldDiscover(entry.idText, dir.ancestry) && !dir.ancestry.includes(entry.idText)) {
    stack.push({
      id: entry.id,
      idText: entry.idText,
      path: entryPath,
      ancestry: [...dir.ancestry, entry.idText],
    })
  }
}

function buildProbeShape(state: ProbeState): SelectionShape {
  if (state.selectedFiles === 0 && state.selectedDirectories === 0 && state.complete) {
    return Object.freeze({ kind: 'empty' })
  }
  if (state.complete && state.singleFile !== undefined && state.selectedFiles === 1 && state.selectedDirectories === 0) {
    return Object.freeze({
      kind: 'single-file',
      file: state.singleFile.file,
      sourcePath: state.singleFile.sourcePath,
      exactBytes: state.singleFile.file.expectedSize,
    })
  }
  return Object.freeze({
    kind: 'tree',
    suggestedName: state.firstRootName ?? DEFAULT_ARCHIVE_NAME,
    knownFileCount: state.selectedFiles,
    knownBytes: state.knownBytes,
    complete: state.complete,
  })
}

interface DiscoveryFrame {
  readonly id: Uint8Array
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly selected: boolean
}

/**
 * Progressively yields selected directories and files without scanning the entire
 * share before transfer begins.
 */
export async function* discoverSelection(
  descriptor: ShareDescriptor,
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  stripSingleFilePrefix: boolean,
  signal: AbortSignal,
  callbacks: DiscoveryCallbacks = {},
): AsyncGenerator<DiscoveredItem, void> {
  const visited = new Set<string>([descriptor.syntheticRootId])
  const queue: DiscoveryFrame[] = [{
    id: descriptor.syntheticRoot,
    idText: descriptor.syntheticRootId,
    path: [],
    ancestry: [descriptor.syntheticRootId],
    selected: selection.defaultSelected,
  }]

  while (queue.length > 0) {
    signal.throwIfAborted()
    const current = queue.shift()!
    const committed = await loadDiscoveryDirectory(catalog, current, signal, callbacks)
    if (committed === undefined) continue
    if (current.selected && committed.omittedCount > 0n) {
      callbacks.onOmittedEntries?.(committed.omittedCount)
    }
    yield* scanDiscoveryDirectory(catalog, selection, current, committed, stripSingleFilePrefix, visited, queue, signal, callbacks)
  }
}

async function loadDiscoveryDirectory(
  catalog: CatalogClient,
  current: DiscoveryFrame,
  signal: AbortSignal,
  callbacks: DiscoveryCallbacks,
): Promise<CommittedDir | undefined> {
  try {
    return await catalog.loadDirectory(current.id, { signal })
  } catch (error) {
    if (signal.aborted) throw error
    callbacks.onDirectoryFailed?.(current.idText, error)
    return undefined
  }
}

async function* scanDiscoveryDirectory(
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  current: DiscoveryFrame,
  committed: CommittedDir,
  stripSingleFilePrefix: boolean,
  visited: Set<string>,
  queue: DiscoveryFrame[],
  signal: AbortSignal,
  callbacks: DiscoveryCallbacks,
): AsyncGenerator<DiscoveredItem, void> {
  for (let pageIndex = 0; pageIndex < committed.pageCount; pageIndex++) {
    signal.throwIfAborted()
    const page = await catalog.page(committed, pageIndex, signal)
    for (const entry of page.entries) {
      signal.throwIfAborted()
      const emitted = classifyDiscoveryEntry(selection, current, entry, stripSingleFilePrefix, visited, queue, callbacks)
      if (emitted !== undefined) yield emitted
    }
  }
}

function classifyDiscoveryEntry(
  selection: FrozenSelectionPolicy,
  current: DiscoveryFrame,
  entry: CatalogEntry,
  stripSingleFilePrefix: boolean,
  visited: Set<string>,
  queue: DiscoveryFrame[],
  callbacks: DiscoveryCallbacks,
): DiscoveredItem | undefined {
  const sourcePath = snapshotCatalogPath([...current.path, entry.name])
  const selected = selection.selected(entry, current.ancestry)
  if (entry.kind === 'file') {
    if (!selected) return undefined
    const relativePath = stripSingleFilePrefix ? Object.freeze([entry.name]) : sourcePath
    const item: DiscoveredFile = Object.freeze({ kind: 'file', entry, sourcePath, relativePath })
    callbacks.onFileDiscovered?.(item)
    return item
  }
  if (visited.has(entry.idText)) return undefined
  visited.add(entry.idText)
  if (selection.shouldDiscover(entry.idText, current.ancestry)) {
    queue.push({
      id: entry.id,
      idText: entry.idText,
      path: sourcePath,
      ancestry: [...current.ancestry, entry.idText],
      selected,
    })
  }
  return selected
    ? Object.freeze({ kind: 'directory', entry, sourcePath, relativePath: sourcePath, selected: true })
    : undefined
}
