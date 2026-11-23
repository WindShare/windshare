import type { CatalogClient } from '../catalog/client'
import { CATALOG_PATH_DEPTH, snapshotCatalogPath } from '../catalog/path-policy'
import type { CatalogEntry, ShareDescriptor } from '../catalog/records'
import type { FrozenSelectionPolicy } from '../catalog/selection'
import { encodeBase64Url } from '../crypto/bytes'

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
      /**
       * Source catalog path prefix anchored as the result root (e.g. `['Photos']`
       * or `['Photos', 'Trip']`), or `[]` for a synthetic multi-root wrapper.
       */
      rootPath: readonly string[]
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
// Bound unhinted fallback directory reads when locating explicit targets without
// valid ancestry hints, so offer probing never scans an entire share upfront.
const SHAPE_FALLBACK_DIRECTORY_LIMIT = 4
// Bound guided ancestor verification to the maximum legal catalog depth plus root
// so deep explicitly selected targets still resolve without allowing unbounded walks.
const SHAPE_GUIDED_DIRECTORY_LIMIT = CATALOG_PATH_DEPTH + 1
const EMPTY_PATH: readonly string[] = Object.freeze([])

type CommittedDir = Awaited<ReturnType<CatalogClient['loadDirectory']>>

interface PositiveTarget {
  readonly kind: 'directory' | 'file'
  readonly parentAncestry: readonly string[] | undefined
}

interface TraversableFrame {
  readonly id: Uint8Array
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly selected: boolean
}

interface ProbeFrame extends TraversableFrame {
  readonly committed?: CommittedDir
}

interface ProbeState {
  singleFile: { file: Extract<CatalogEntry, { kind: 'file' }>; sourcePath: readonly string[] } | undefined
  selectedFiles: number
  selectedDirectories: number
  knownBytes: bigint
  commonRoot: readonly string[] | undefined
  unprobedRoot: readonly string[] | undefined
  failedUnselectedRoot: readonly string[] | undefined
  firstDirectoryError: unknown
  complete: boolean
}

interface ProbeBudget {
  fallbackScanned: number
  guidedScanned: number
}

/**
 * Tracks unmatched positive (`selected: true`) overrides across authenticated pages.
 * Catalog node IDs are unique within a share, so once every positive override is
 * observed, no unselected directory can contain another selected node. Draft
 * ancestry paths act only as search-priority hints; every traversed edge is still
 * verified against authenticated pages and fallback directories remain eligible
 * while any target is unmatched.
 */
class SelectionDiscoveryTracker {
  readonly #remaining = new Map<string, PositiveTarget>()

  constructor(selection: FrozenSelectionPolicy, syntheticRootId: string) {
    const hints = buildParentAncestryHints(selection, syntheticRootId)
    for (const rule of selection.canonicalRules) {
      if (!rule.selected) continue
      const idText = encodeBase64Url(rule.id)
      // Discovery always begins at the synthetic root itself.
      if (rule.kind === 'directory' && idText === syntheticRootId) continue
      this.#remaining.set(idText, {
        kind: rule.kind,
        parentAncestry: hints.get(ruleHintKey(rule.kind, idText)),
      })
    }
  }

  get hasRemainingTargets(): boolean {
    return this.#remaining.size > 0
  }

  observe(entry: CatalogEntry): void {
    const target = this.#remaining.get(entry.idText)
    if (target !== undefined && target.kind === entry.kind) {
      this.#remaining.delete(entry.idText)
    }
  }

  isHintedDirectory(directoryAncestry: readonly string[]): boolean {
    for (const target of this.#remaining.values()) {
      if (target.parentAncestry !== undefined && isSegmentPrefix(directoryAncestry, target.parentAncestry)) {
        return true
      }
    }
    return false
  }
}

class DiscoveryQueues<T extends TraversableFrame> {
  readonly #guided: T[] = []
  readonly #selected: T[] = []
  readonly #fallback: T[] = []

  enqueue(frame: T, hinted: boolean): void {
    if (hinted) {
      this.#guided.push(frame)
    } else if (frame.selected) {
      this.#selected.push(frame)
    } else {
      this.#fallback.push(frame)
    }
  }

  next(tracker: SelectionDiscoveryTracker): { readonly frame: T; readonly guided: boolean } | undefined {
    const guided = this.#nextGuided(tracker)
    if (guided !== undefined) return { frame: guided, guided: true }
    if (this.#selected.length > 0) {
      return { frame: this.#selected.shift()!, guided: false }
    }
    if (!tracker.hasRemainingTargets) {
      this.#fallback.length = 0
      return undefined
    }
    if (this.#fallback.length > 0) {
      return { frame: this.#fallback.shift()!, guided: false }
    }
    return undefined
  }

  forEachPendingPath(
    tracker: SelectionDiscoveryTracker,
    visit: (path: readonly string[]) => void,
  ): void {
    for (const frame of this.#guided) {
      if (frame.selected || tracker.hasRemainingTargets) visit(frame.path)
    }
    for (const frame of this.#selected) visit(frame.path)
    if (tracker.hasRemainingTargets) {
      for (const frame of this.#fallback) visit(frame.path)
    }
  }

  #nextGuided(tracker: SelectionDiscoveryTracker): T | undefined {
    while (this.#guided.length > 0) {
      const frame = this.#guided.pop()!
      if (tracker.isHintedDirectory(frame.ancestry)) return frame
      if (frame.selected) {
        this.#selected.push(frame)
      } else if (tracker.hasRemainingTargets) {
        this.#fallback.push(frame)
      }
    }
    return undefined
  }
}

function ruleHintKey(kind: 'directory' | 'file', idText: string): string {
  return `${kind}:${idText}`
}

function buildParentAncestryHints(
  selection: FrozenSelectionPolicy,
  syntheticRootId: string,
): Map<string, readonly string[]> {
  const hints = new Map<string, readonly string[]>()
  if (selection.draftRules === undefined) return hints
  for (const rule of selection.draftRules) {
    if (!rule.selected) continue
    const idText = encodeBase64Url(rule.id)
    const parentAncestry = normalizeParentAncestry(rule.kind, idText, rule.ancestry, syntheticRootId)
    if (parentAncestry !== undefined) {
      hints.set(ruleHintKey(rule.kind, idText), parentAncestry)
    }
  }
  return hints
}

function normalizeParentAncestry(
  kind: 'directory' | 'file',
  idText: string,
  ancestry: readonly string[],
  syntheticRootId: string,
): readonly string[] | undefined {
  if (ancestry.length === 0 || ancestry[0] !== syntheticRootId) return undefined
  if (kind === 'file') return ancestry
  if (ancestry.length < 2 || ancestry.at(-1) !== idText) return undefined
  return ancestry.slice(0, -1)
}

function isSegmentPrefix(prefix: readonly string[], full: readonly string[]): boolean {
  if (prefix.length > full.length) return false
  for (let index = 0; index < prefix.length; index++) {
    if (prefix[index] !== full[index]) return false
  }
  return true
}

function shouldTraverseDirectory(
  selection: FrozenSelectionPolicy,
  tracker: SelectionDiscoveryTracker,
  directoryId: string,
  parentAncestry: readonly string[],
  selected: boolean,
): boolean {
  if (selected) return true
  return tracker.hasRemainingTargets && selection.shouldDiscover(directoryId, parentAncestry)
}

/**
 * Classifies the user's selection (empty / single file / tree) and anchors its
 * result root without descending into already-selected directories. Directory
 * contents and size totals are discovered progressively during transfer.
 */
export async function inspectSelectionShape(
  descriptor: ShareDescriptor,
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  signal: AbortSignal,
): Promise<SelectionShape> {
  signal.throwIfAborted()
  const rootSelected = selection.directorySelected(descriptor.syntheticRootId, [])
  const tracker = new SelectionDiscoveryTracker(selection, descriptor.syntheticRootId)
  if (!rootSelected && !tracker.hasRemainingTargets) {
    return Object.freeze({ kind: 'empty' })
  }

  const rootCommitted = await catalog.loadDirectory(descriptor.syntheticRoot, { signal })
  if (rootCommitted.entryCount === 0) {
    return Object.freeze({ kind: 'empty' })
  }

  const rootAncestry = [descriptor.syntheticRootId]
  // Fast path: sole root file selected.
  if (rootCommitted.entryCount === 1) {
    const sole = await readFirstEntry(catalog, rootCommitted, signal)
    if (sole?.kind === 'file') {
      if (!selection.selected(sole, rootAncestry)) return Object.freeze({ kind: 'empty' })
      return Object.freeze({
        kind: 'single-file',
        file: sole,
        sourcePath: Object.freeze([sole.name]),
        exactBytes: sole.expectedSize,
      })
    }
  }

  return probeSelectionSubtree(descriptor, catalog, selection, tracker, rootSelected, rootCommitted, signal)
}

async function readFirstEntry(
  catalog: CatalogClient,
  committed: CommittedDir,
  signal: AbortSignal,
): Promise<CatalogEntry | undefined> {
  for (let pageIndex = 0; pageIndex < committed.pageCount; pageIndex++) {
    signal.throwIfAborted()
    const page = await catalog.page(committed, pageIndex, signal)
    if (page.entries.length > 0) return page.entries[0]
  }
  return undefined
}

async function probeSelectionSubtree(
  descriptor: ShareDescriptor,
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  tracker: SelectionDiscoveryTracker,
  rootSelected: boolean,
  rootCommitted: CommittedDir,
  signal: AbortSignal,
): Promise<SelectionShape> {
  const state: ProbeState = {
    singleFile: undefined,
    selectedFiles: 0,
    selectedDirectories: 0,
    knownBytes: 0n,
    commonRoot: undefined,
    unprobedRoot: undefined,
    failedUnselectedRoot: undefined,
    firstDirectoryError: undefined,
    complete: true,
  }
  const visited = new Set<string>([descriptor.syntheticRootId])
  const queues = new DiscoveryQueues<ProbeFrame>()
  const rootFrame: ProbeFrame = {
    id: descriptor.syntheticRoot,
    idText: descriptor.syntheticRootId,
    path: EMPTY_PATH,
    ancestry: [descriptor.syntheticRootId],
    selected: rootSelected,
    committed: rootCommitted,
  }
  queues.enqueue(rootFrame, tracker.isHintedDirectory(rootFrame.ancestry))

  const budget: ProbeBudget = { fallbackScanned: 0, guidedScanned: 0 }
  while (true) {
    signal.throwIfAborted()
    const next = queues.next(tracker)
    if (next === undefined) break
    // A selected directory enqueued only for a hinted descendant becomes redundant
    // once that hint is satisfied elsewhere; never load its contents.
    if (next.frame.selected && next.frame.committed === undefined && !next.guided) continue
    if (!admitProbeDirectory(next.guided, budget)) {
      state.complete = false
      state.unprobedRoot = mergePathPrefix(state.unprobedRoot, next.frame.path)
      queues.forEachPendingPath(tracker, path => {
        state.unprobedRoot = mergePathPrefix(state.unprobedRoot, path)
      })
      break
    }
    const committed = await loadProbeDirectory(catalog, next.frame, state, signal)
    if (committed === undefined) continue
    await probeDirectoryPages(catalog, selection, tracker, next.frame, committed, visited, queues, state, signal)
  }

  if (state.selectedFiles === 0 && state.selectedDirectories === 0 && state.firstDirectoryError !== undefined) {
    throw state.firstDirectoryError
  }
  if (tracker.hasRemainingTargets && state.failedUnselectedRoot !== undefined) {
    state.complete = false
    state.unprobedRoot = mergePathPrefix(state.unprobedRoot, state.failedUnselectedRoot)
  }

  return buildProbeShape(state)
}

function admitProbeDirectory(guided: boolean, budget: ProbeBudget): boolean {
  if (guided && budget.guidedScanned < SHAPE_GUIDED_DIRECTORY_LIMIT) {
    budget.guidedScanned += 1
    return true
  }
  if (budget.fallbackScanned >= SHAPE_FALLBACK_DIRECTORY_LIMIT) return false
  budget.fallbackScanned += 1
  return true
}

async function loadProbeDirectory(
  catalog: CatalogClient,
  frame: ProbeFrame,
  state: ProbeState,
  signal: AbortSignal,
): Promise<CommittedDir | undefined> {
  if (frame.committed !== undefined) return frame.committed
  try {
    return await catalog.loadDirectory(frame.id, { signal })
  } catch (error) {
    if (signal.aborted) throw error
    // Tolerate per-directory failures while probing so one unreadable subtree
    // cannot block healthy selected siblings; TransferJob reports the failure.
    state.firstDirectoryError ??= error
    if (!frame.selected) {
      state.failedUnselectedRoot = mergePathPrefix(state.failedUnselectedRoot, frame.path)
    }
    return undefined
  }
}

async function probeDirectoryPages(
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  tracker: SelectionDiscoveryTracker,
  dir: ProbeFrame,
  committed: CommittedDir,
  visited: Set<string>,
  queues: DiscoveryQueues<ProbeFrame>,
  state: ProbeState,
  signal: AbortSignal,
): Promise<void> {
  for (let pageIndex = 0; pageIndex < committed.pageCount; pageIndex++) {
    signal.throwIfAborted()
    const page = await catalog.page(committed, pageIndex, signal)
    for (const entry of page.entries) {
      recordProbedEntry(selection, tracker, dir, entry, visited, queues, state)
    }
    if (!dir.selected && !tracker.hasRemainingTargets) return
    if (
      pageIndex + 1 < committed.pageCount &&
      dir.selected &&
      !tracker.hasRemainingTargets &&
      state.selectedDirectories > 0 &&
      state.commonRoot !== undefined &&
      isSegmentPrefix(state.commonRoot, dir.path)
    ) {
      state.complete = false
      return
    }
  }
}

function recordProbedEntry(
  selection: FrozenSelectionPolicy,
  tracker: SelectionDiscoveryTracker,
  dir: ProbeFrame,
  entry: CatalogEntry,
  visited: Set<string>,
  queues: DiscoveryQueues<ProbeFrame>,
  state: ProbeState,
): void {
  tracker.observe(entry)
  const entryPath = snapshotCatalogPath([...dir.path, entry.name])
  const selected = selection.selected(entry, dir.ancestry)
  if (entry.kind === 'file') {
    if (!selected) return
    state.selectedFiles += 1
    state.knownBytes += entry.expectedSize
    extendCommonRoot(state, dir.path)
    state.singleFile = state.selectedFiles === 1 && state.selectedDirectories === 0
      ? { file: entry, sourcePath: entryPath }
      : undefined
    return
  }
  if (selected) {
    state.selectedDirectories += 1
    state.singleFile = undefined
    state.complete = false
    extendCommonRoot(state, entryPath)
  }
  if (visited.has(entry.idText) || dir.ancestry.includes(entry.idText)) return
  visited.add(entry.idText)
  const childAncestry = [...dir.ancestry, entry.idText]
  const hinted = tracker.isHintedDirectory(childAncestry)
  // A selected directory already settles the shape as a tree anchored at or above
  // its own path; only descend into it if a hinted descendant target must be matched
  // to avoid fallback scans across unselected siblings.
  if (selected && !hinted) return
  if (!selected && !shouldTraverseDirectory(selection, tracker, entry.idText, dir.ancestry, false)) return
  queues.enqueue({
    id: entry.id,
    idText: entry.idText,
    path: entryPath,
    ancestry: childAncestry,
    selected,
  }, hinted)
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
  const rootPath = resolveProbeRootPath(state)
  return Object.freeze({
    kind: 'tree',
    suggestedName: rootPath.at(-1) ?? DEFAULT_ARCHIVE_NAME,
    rootPath,
    knownFileCount: state.selectedFiles,
    knownBytes: state.knownBytes,
    complete: state.complete,
  })
}

function mergePathPrefix(
  current: readonly string[] | undefined,
  next: readonly string[],
): readonly string[] {
  return current === undefined ? next : commonPathPrefix(current, next)
}

function extendCommonRoot(state: ProbeState, directoryPath: readonly string[]): void {
  state.commonRoot = mergePathPrefix(state.commonRoot, directoryPath)
}

function commonPathPrefix(
  left: readonly string[],
  right: readonly string[],
): readonly string[] {
  const limit = Math.min(left.length, right.length)
  let matched = 0
  while (matched < limit && left[matched] === right[matched]) matched += 1
  if (matched === left.length) return left
  if (matched === right.length) return right
  return Object.freeze(left.slice(0, matched))
}

function resolveProbeRootPath(state: ProbeState): readonly string[] {
  let root = state.commonRoot
  if (root === undefined) return EMPTY_PATH
  if (state.unprobedRoot !== undefined) {
    root = commonPathPrefix(root, state.unprobedRoot)
  }
  return root.length === 0 ? EMPTY_PATH : root
}

type DiscoveryFrame = TraversableFrame

/**
 * Progressively yields selected directories and files without scanning the entire
 * share before transfer begins.
 */
export async function* discoverSelection(
  descriptor: ShareDescriptor,
  catalog: CatalogClient,
  selection: FrozenSelectionPolicy,
  rootPath: readonly string[],
  signal: AbortSignal,
  callbacks: DiscoveryCallbacks = {},
): AsyncGenerator<DiscoveredItem, void> {
  const tracker = new SelectionDiscoveryTracker(selection, descriptor.syntheticRootId)
  const visited = new Set<string>([descriptor.syntheticRootId])
  const queues = new DiscoveryQueues<DiscoveryFrame>()
  const rootFrame: DiscoveryFrame = {
    id: descriptor.syntheticRoot,
    idText: descriptor.syntheticRootId,
    path: EMPTY_PATH,
    ancestry: [descriptor.syntheticRootId],
    selected: selection.directorySelected(descriptor.syntheticRootId, []),
  }
  queues.enqueue(rootFrame, tracker.isHintedDirectory(rootFrame.ancestry))

  while (true) {
    signal.throwIfAborted()
    const next = queues.next(tracker)
    if (next === undefined) break
    const current = next.frame
    const committed = await loadDiscoveryDirectory(catalog, current, signal, callbacks)
    if (committed === undefined) continue
    if (current.selected && committed.omittedCount > 0n) {
      callbacks.onOmittedEntries?.(committed.omittedCount)
    }
    yield* scanDiscoveryDirectory(
      catalog,
      selection,
      tracker,
      current,
      committed,
      rootPath,
      visited,
      queues,
      signal,
      callbacks,
    )
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
  tracker: SelectionDiscoveryTracker,
  current: DiscoveryFrame,
  committed: CommittedDir,
  rootPath: readonly string[],
  visited: Set<string>,
  queues: DiscoveryQueues<DiscoveryFrame>,
  signal: AbortSignal,
  callbacks: DiscoveryCallbacks,
): AsyncGenerator<DiscoveredItem, void> {
  for (let pageIndex = 0; pageIndex < committed.pageCount; pageIndex++) {
    signal.throwIfAborted()
    const page = await catalog.page(committed, pageIndex, signal)
    for (const entry of page.entries) {
      signal.throwIfAborted()
      const emitted = classifyDiscoveryEntry(
        selection,
        tracker,
        current,
        entry,
        rootPath,
        visited,
        queues,
        callbacks,
      )
      if (emitted !== undefined) yield emitted
    }
    if (!current.selected && !tracker.hasRemainingTargets) return
  }
}

function classifyDiscoveryEntry(
  selection: FrozenSelectionPolicy,
  tracker: SelectionDiscoveryTracker,
  current: DiscoveryFrame,
  entry: CatalogEntry,
  rootPath: readonly string[],
  visited: Set<string>,
  queues: DiscoveryQueues<DiscoveryFrame>,
  callbacks: DiscoveryCallbacks,
): DiscoveredItem | undefined {
  tracker.observe(entry)
  const sourcePath = snapshotCatalogPath([...current.path, entry.name])
  const selected = selection.selected(entry, current.ancestry)
  if (entry.kind === 'file') {
    if (!selected) return undefined
    const stripped = stripRootPath(sourcePath, rootPath)
    const relativePath = stripped.length > 0 ? stripped : Object.freeze([entry.name])
    const item: DiscoveredFile = Object.freeze({ kind: 'file', entry, sourcePath, relativePath })
    callbacks.onFileDiscovered?.(item)
    return item
  }
  if (visited.has(entry.idText)) return undefined
  visited.add(entry.idText)
  if (shouldTraverseDirectory(selection, tracker, entry.idText, current.ancestry, selected)) {
    const childAncestry = [...current.ancestry, entry.idText]
    queues.enqueue({
      id: entry.id,
      idText: entry.idText,
      path: sourcePath,
      ancestry: childAncestry,
      selected,
    }, tracker.isHintedDirectory(childAncestry))
  }
  return selected
    ? Object.freeze({
        kind: 'directory',
        entry,
        sourcePath,
        relativePath: stripRootPath(sourcePath, rootPath),
        selected: true,
      })
    : undefined
}

function stripRootPath(
  sourcePath: readonly string[],
  rootPath: readonly string[],
): readonly string[] {
  if (rootPath.length === 0 || sourcePath.length < rootPath.length) return sourcePath
  for (let index = 0; index < rootPath.length; index += 1) {
    if (sourcePath[index] !== rootPath[index]) return sourcePath
  }
  return Object.freeze(sourcePath.slice(rootPath.length))
}
