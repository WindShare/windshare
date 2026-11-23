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
// Bound guided ancestor verification to the maximum legal catalog depth plus root
// so deep explicitly selected targets still resolve without allowing unbounded walks.
const SHAPE_GUIDED_DIRECTORY_LIMIT = CATALOG_PATH_DEPTH + 1

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
  firstRootName: string | undefined
  complete: boolean
}

interface ProbeBudget {
  regularScanned: number
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
      if (target.parentAncestry !== undefined && isAncestryPrefix(directoryAncestry, target.parentAncestry)) {
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

function isAncestryPrefix(prefix: readonly string[], full: readonly string[]): boolean {
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
  const tracker = new SelectionDiscoveryTracker(selection, descriptor.syntheticRootId)
  const visited = new Set<string>([descriptor.syntheticRootId])
  const queues = new DiscoveryQueues<ProbeFrame>()
  const rootFrame: ProbeFrame = {
    id: descriptor.syntheticRoot,
    idText: descriptor.syntheticRootId,
    path: [],
    ancestry: [descriptor.syntheticRootId],
    selected: selection.directorySelected(descriptor.syntheticRootId, []),
    committed: rootCommitted,
  }
  queues.enqueue(rootFrame, tracker.isHintedDirectory(rootFrame.ancestry))

  const budget: ProbeBudget = { regularScanned: 0, guidedScanned: 0 }
  while (state.complete) {
    signal.throwIfAborted()
    const next = queues.next(tracker)
    if (next === undefined) break
    if (!admitProbeDirectory(next.guided, state, budget)) {
      state.complete = false
      break
    }
    const committed = next.frame.committed ?? await catalog.loadDirectory(next.frame.id, { signal })
    await probeDirectoryPages(catalog, selection, tracker, next.frame, committed, visited, queues, state, signal)
  }

  return buildProbeShape(state)
}

function admitProbeDirectory(guided: boolean, state: ProbeState, budget: ProbeBudget): boolean {
  const canUseGuidedBudget = guided &&
    state.selectedDirectories === 0 &&
    state.selectedFiles <= 1 &&
    budget.guidedScanned < SHAPE_GUIDED_DIRECTORY_LIMIT
  if (canUseGuidedBudget) {
    budget.guidedScanned += 1
    return true
  }
  if (budget.regularScanned >= SHAPE_PROBE_DIRECTORY_LIMIT) return false
  budget.regularScanned += 1
  return true
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
    if (pageIndex + 1 < committed.pageCount && state.selectedDirectories > 0 && state.selectedFiles > 1) {
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
  if (visited.has(entry.idText) || dir.ancestry.includes(entry.idText)) return
  visited.add(entry.idText)
  if (!shouldTraverseDirectory(selection, tracker, entry.idText, dir.ancestry, selected)) return
  const childAncestry = [...dir.ancestry, entry.idText]
  queues.enqueue({
    id: entry.id,
    idText: entry.idText,
    path: entryPath,
    ancestry: childAncestry,
    selected,
  }, tracker.isHintedDirectory(childAncestry))
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

type DiscoveryFrame = TraversableFrame

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
  const tracker = new SelectionDiscoveryTracker(selection, descriptor.syntheticRootId)
  const visited = new Set<string>([descriptor.syntheticRootId])
  const queues = new DiscoveryQueues<DiscoveryFrame>()
  const rootFrame: DiscoveryFrame = {
    id: descriptor.syntheticRoot,
    idText: descriptor.syntheticRootId,
    path: [],
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
      stripSingleFilePrefix,
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
  stripSingleFilePrefix: boolean,
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
        stripSingleFilePrefix,
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
  stripSingleFilePrefix: boolean,
  visited: Set<string>,
  queues: DiscoveryQueues<DiscoveryFrame>,
  callbacks: DiscoveryCallbacks,
): DiscoveredItem | undefined {
  tracker.observe(entry)
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
    ? Object.freeze({ kind: 'directory', entry, sourcePath, relativePath: sourcePath, selected: true })
    : undefined
}
