import type {
  CapabilityLink,
  ManifestEntry,
  TransferPlan,
  ValidatedManifestV1,
} from '../contracts'
import { quotePathForDiagnostic } from '../manifest'

export type ReceiverPhase =
  | 'awaiting-key'
  | 'joining'
  | 'planning'
  | 'ready'
  | 'preparing-output'
  | 'transferring'
  | 'reconnecting'
  | 'aborting'
  | 'completed'
  | 'failed'
  | 'aborted'

export type OutputChoiceId = 'folder' | 'download'

export interface OutputChoice {
  readonly id: OutputChoiceId
  readonly label: string
  readonly description: string
  readonly available: boolean
}

export interface SelectionRow {
  readonly path: string
  readonly name: string
  readonly accessibleLabel: string
  readonly kind: ManifestEntry['kind']
  readonly indentLevel: number
  readonly selected: boolean
  readonly partial: boolean
}

export interface TransferProgress {
  readonly writtenBytes: number
  readonly totalBytes: number
  readonly completedBlocks: number
  readonly totalBlocks: number
  readonly retryBlocks: number
  readonly channels: number
  readonly bufferedBlocks: number
  readonly maxBufferedBlocks: number
}

export interface ReceiverSnapshot {
  readonly phase: ReceiverPhase
  readonly status: string
  readonly error: string | null
  readonly entries: readonly SelectionRow[]
  readonly manifestEntryCount: number
  /** Zero-based page index for the bounded selection-row window. */
  readonly selectionPageIndex: number
  readonly selectionPageCount: number
  readonly selectedBytes: number
  readonly selectedEntryCount: number
  readonly outputChoices: readonly OutputChoice[]
  readonly outputChoice: OutputChoiceId
  readonly progress: TransferProgress
  readonly reconnectAttempt: number
  readonly canStart: boolean
}

export interface JoinedShare {
  readonly manifest: ValidatedManifestV1
  close(): Promise<void>
}

export interface ReceiverTransferObserver {
  started(progress: TransferProgress): void
  progress(progress: TransferProgress): void
  reconnecting(attempt: number): void
  reconnected(progress: TransferProgress): void
}

/**
 * Join and plan operations are safe before a user gesture. `start` is the sole
 * capability boundary for output pickers, requests, signaling, offers, and ICE.
 */
export interface ReceiverGateway {
  readonly outputChoices: readonly OutputChoice[]
  /**
   * The caller destroys its transient capability bytes as soon as this method
   * returns. Implementations must therefore snapshot any asynchronously needed
   * key material before returning the promise.
   */
  join(capability: CapabilityLink, signal: AbortSignal): Promise<JoinedShare>
  compileSelection(
    share: JoinedShare,
    selectors: readonly string[] | null,
    signal: AbortSignal,
  ): Promise<TransferPlan>
  start(
    share: JoinedShare,
    plan: TransferPlan,
    outputChoice: OutputChoiceId,
    observer: ReceiverTransferObserver,
    signal: AbortSignal,
  ): Promise<void>
}

export type ReceiverPublicErrorCode =
  | 'connection-failed'
  | 'invalid-capability'
  | 'manifest-changed'
  | 'missing-relay'
  | 'output-cancelled'
  | 'output-unavailable'
  | 'peer-terminal'
  | 'transfer-failed'

export class ReceiverPublicError extends Error {
  readonly code: ReceiverPublicErrorCode

  constructor(code: ReceiverPublicErrorCode, message: string) {
    super(message)
    this.name = 'ReceiverPublicError'
    this.code = code
  }
}

interface SelectionRange {
  readonly entry: ManifestEntry
  firstUnit: number
  endUnit: number
  unit: number | undefined
}

const MAX_SELECTION_INDENT_LEVEL = 16
const MAX_ENTRY_NAME_CHARACTERS = 160
export const SELECTION_PAGE_ROWS = 200

function comparePaths(left: string, right: string): number {
  if (left < right) {
    return -1
  }
  return left > right ? 1 : 0
}

function lowerBound(paths: readonly string[], target: string): number {
  let low = 0
  let high = paths.length
  while (low < high) {
    const middle = low + Math.floor((high - low) / 2)
    const path = paths[middle]
    if (path !== undefined && comparePaths(path, target) < 0) {
      low = middle + 1
    } else {
      high = middle
    }
  }
  return low
}

function hasDescendant(sortedPaths: readonly string[], path: string): boolean {
  const prefix = `${path}/`
  return sortedPaths[lowerBound(sortedPaths, prefix)]?.startsWith(prefix) === true
}

function indentationLevel(path: string): number {
  let level = 0
  for (let index = 0; index < path.length; index += 1) {
    if (path[index] === '/') {
      level += 1
      if (level === MAX_SELECTION_INDENT_LEVEL) {
        return level
      }
    }
  }
  return level
}

function displayName(path: string): string {
  const start = path.lastIndexOf('/') + 1
  const end = Math.min(path.length, start + MAX_ENTRY_NAME_CHARACTERS)
  return `${path.slice(start, end)}${end < path.length ? '…' : ''}`
}

function buildSelectedPrefix(selection: readonly boolean[]): Uint32Array {
  const prefix = new Uint32Array(selection.length + 1)
  for (let index = 0; index < selection.length; index += 1) {
    prefix[index + 1] = (prefix[index] ?? 0) + (selection[index] === true ? 1 : 0)
  }
  return prefix
}

function selectedInRange(
  prefix: Uint32Array,
  range: SelectionRange,
): number {
  return (prefix[range.endUnit] ?? 0) - (prefix[range.firstUnit] ?? 0)
}

/**
 * Sorted unit ranges avoid allocating one object per path segment and avoid call
 * stack growth for hostile deep paths. Files and empty directories are the
 * independent units; non-empty directory state is derived from descendant units.
 */
export class EntrySelectionModel {
  readonly #entries: readonly ManifestEntry[]
  readonly #rangeByPath = new Map<string, SelectionRange>()
  readonly #orderedRanges: readonly SelectionRange[]
  readonly #units: readonly ManifestEntry[]
  readonly #prefixBySelection = new WeakMap<readonly boolean[], Uint32Array>()

  constructor(entries: readonly ManifestEntry[]) {
    this.#entries = Object.freeze([...entries])
    const orderedEntries = [...this.#entries].sort((left, right) =>
      comparePaths(left.path, right.path),
    )
    const orderedPaths = orderedEntries.map((entry) => entry.path)
    const units = orderedEntries.filter(
      (entry) => entry.kind === 'file' || !hasDescendant(orderedPaths, entry.path),
    )
    const unitPaths = units.map((entry) => entry.path)

    for (const entry of this.#entries) {
      const independent = entry.kind === 'file' || !hasDescendant(orderedPaths, entry.path)
      const firstUnit = lowerBound(unitPaths, independent ? entry.path : `${entry.path}/`)
      const range = {
        entry,
        firstUnit,
        endUnit: independent ? firstUnit + 1 : lowerBound(unitPaths, `${entry.path}0`),
        unit: independent ? firstUnit : undefined,
      }
      this.#rangeByPath.set(entry.path, range)
    }
    this.#orderedRanges = Object.freeze(
      orderedEntries.map((entry) => {
        const range = this.#rangeByPath.get(entry.path)
        if (range === undefined) {
          throw new TypeError('Manifest selection range is missing')
        }
        return range
      }),
    )
    this.#units = Object.freeze(units)
  }

  get entryCount(): number {
    return this.#entries.length
  }

  defaultSelection(): readonly boolean[] {
    return Object.freeze(new Array<boolean>(this.#units.length).fill(true))
  }

  toggle(selection: readonly boolean[], path: string): readonly boolean[] {
    this.#requireSelection(selection)
    const range = this.#rangeByPath.get(path)
    if (range === undefined || range.endUnit === range.firstUnit) {
      return selection
    }
    const prefix = this.#selectedPrefix(selection)
    const rangeLength = range.endUnit - range.firstUnit
    const nextValue = selectedInRange(prefix, range) !== rangeLength
    const next = [...selection]
    for (let index = range.firstUnit; index < range.endUnit; index += 1) {
      next[index] = nextValue
    }
    return Object.freeze(next)
  }

  rowsWindow(
    selection: readonly boolean[],
    firstEntry: number,
    maximumRows: number,
  ): readonly SelectionRow[] {
    this.#requireSelection(selection)
    if (!Number.isSafeInteger(firstEntry) || firstEntry < 0) {
      throw new RangeError('Selection row offset must be a non-negative safe integer')
    }
    if (!Number.isSafeInteger(maximumRows) || maximumRows <= 0) {
      throw new RangeError('Selection row limit must be a positive safe integer')
    }
    const prefix = this.#selectedPrefix(selection)
    const endEntry = Math.min(this.#entries.length, firstEntry + maximumRows)
    return Object.freeze(
      this.#entries.slice(firstEntry, endEntry).map((entry) => {
        const range = this.#rangeByPath.get(entry.path)
        const rangeLength = range === undefined ? 0 : range.endUnit - range.firstUnit
        const picked = range === undefined ? 0 : selectedInRange(prefix, range)
        const kindLabel = entry.kind === 'directory' ? 'Folder' : 'File'
        return Object.freeze({
          path: entry.path,
          name: displayName(entry.path),
          accessibleLabel: `${kindLabel} ${quotePathForDiagnostic(entry.path)}`,
          kind: entry.kind,
          indentLevel: indentationLevel(entry.path),
          selected: rangeLength > 0 && picked === rangeLength,
          partial: picked > 0 && picked < rangeLength,
        })
      }),
    )
  }

  selectors(selection: readonly boolean[]): readonly string[] | null {
    this.#requireSelection(selection)
    if (selection.every(Boolean)) {
      return null
    }
    const prefix = this.#selectedPrefix(selection)
    const selectors: string[] = []
    let coveredPrefix: string | undefined
    for (const range of this.#orderedRanges) {
      const path = range.entry.path
      if (coveredPrefix !== undefined) {
        if (path.startsWith(coveredPrefix)) {
          continue
        }
        coveredPrefix = undefined
      }
      const rangeLength = range.endUnit - range.firstUnit
      if (rangeLength === 0 || selectedInRange(prefix, range) !== rangeLength) {
        continue
      }
      selectors.push(path)
      if (range.entry.kind === 'directory') {
        coveredPrefix = `${path}/`
      }
    }
    return Object.freeze(selectors)
  }

  selectedBytes(selection: readonly boolean[]): number {
    this.#requireSelection(selection)
    let bytes = 0
    for (let index = 0; index < this.#units.length; index += 1) {
      const entry = this.#units[index]
      if (selection[index] === true && entry?.kind === 'file') {
        bytes += entry.size
      }
    }
    return bytes
  }

  selectedEntryCount(selection: readonly boolean[]): number {
    this.#requireSelection(selection)
    const prefix = this.#selectedPrefix(selection)
    let count = 0
    for (const range of this.#orderedRanges) {
      const rangeLength = range.endUnit - range.firstUnit
      if (rangeLength > 0 && selectedInRange(prefix, range) === rangeLength) {
        count += 1
      }
    }
    return count
  }

  #requireSelection(selection: readonly boolean[]): void {
    if (selection.length !== this.#units.length) {
      throw new TypeError('Selection does not belong to this manifest')
    }
  }

  #selectedPrefix(selection: readonly boolean[]): Uint32Array {
    if (!Object.isFrozen(selection)) {
      // Only model-owned frozen selections have stable identity. A mutable test or
      // adapter projection must never receive stale aggregate state from the cache.
      return buildSelectedPrefix(selection)
    }
    const cached = this.#prefixBySelection.get(selection)
    if (cached !== undefined) {
      return cached
    }
    // Page projection, selector compilation, and aggregate counts share this
    // index so a wide manifest never allocates several full-size prefixes per UI turn.
    const prefix = buildSelectedPrefix(selection)
    this.#prefixBySelection.set(selection, prefix)
    return prefix
  }
}

export function emptyProgress(): TransferProgress {
  return Object.freeze({
    writtenBytes: 0,
    totalBytes: 0,
    completedBlocks: 0,
    totalBlocks: 0,
    retryBlocks: 0,
    channels: 0,
    bufferedBlocks: 0,
    maxBufferedBlocks: 0,
  })
}
