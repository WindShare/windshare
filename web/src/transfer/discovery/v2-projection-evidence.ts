import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import type { V2CommittedDirectory } from '../../catalog/v2-page-store'
import type { V2CatalogEntry, V2CatalogPage } from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  MAX_PROJECTION_SELECTED_ROOT_FACTS,
  SelectionProjectionError,
  createAuthenticatedProjectionEvidence,
  selectionTargetKey,
  type AuthenticatedProjectionEvidence,
  type ProjectedFileFact,
  type SelectedRootFact,
  type UnsettledSelectionTarget,
} from '../projection/model'

export interface V2ProjectionGenerationInput {
  readonly committed: V2CommittedDirectory
  readonly pages: AsyncIterable<V2CatalogPage>
  readonly selection: V2FrozenSelectionPolicy
  /** IDs from the synthetic root through the containing directory. */
  readonly directoryAncestry: readonly string[]
  /** Portable path segments of the containing directory; empty means synthetic root. */
  readonly directoryPath: readonly string[]
  readonly containingDirectorySelected: boolean
  readonly unsettledTargets: readonly UnsettledSelectionTarget[]
  readonly signal?: AbortSignal
}

/**
 * Committed pages are streamed into a compact proof batch. Entry identity never
 * escapes into a catalog-sized accumulator; only bounded root facts and metrics do.
 */
export async function projectAuthenticatedV2Generation(
  input: V2ProjectionGenerationInput,
): Promise<AuthenticatedProjectionEvidence> {
  requireCommittedGeneration(input.committed)
  const directoryPath = input.directoryPath.length === 0
    ? Object.freeze([]) as readonly string[]
    : snapshotPortableCatalogPath(input.directoryPath)
  const accumulator = new GenerationProjectionAccumulator(input, directoryPath)
  for await (const page of input.pages) {
    input.signal?.throwIfAborted()
    accumulator.accept(page)
  }
  return accumulator.finish()
}

class GenerationProjectionAccumulator {
  readonly #input: V2ProjectionGenerationInput
  readonly #directoryPath: readonly string[]
  readonly #targetByKey: ReadonlyMap<string, UnsettledSelectionTarget>
  readonly #settledTargetKeys = new Set<string>()
  readonly #selectedRoots: SelectedRootFact[] = []
  #selectedRootCount = 0
  #selectedFileCount = 0
  #selectedDirectoryCount = 0
  #selectedBytes = 0n
  #representativeFile: ProjectedFileFact | undefined
  #pageCount = 0
  #entryCount = 0
  #terminalCommitment: Uint8Array<ArrayBuffer> | undefined

  constructor(input: V2ProjectionGenerationInput, directoryPath: readonly string[]) {
    this.#input = input
    this.#directoryPath = directoryPath
    this.#targetByKey = new Map(input.unsettledTargets.map((target) =>
      [selectionTargetKey(target), target]))
  }

  accept(page: V2CatalogPage): void {
    requireCommittedPage(this.#input.committed, page, this.#pageCount)
    this.#pageCount += 1
    this.#entryCount += page.entries.length
    this.#terminalCommitment = page.objectCommitment
    for (const entry of page.entries) this.#observeEntry(entry)
  }

  finish(): AuthenticatedProjectionEvidence {
    requireCompleteReplay(
      this.#input.committed,
      this.#pageCount,
      this.#entryCount,
      this.#terminalCommitment,
    )
    this.#settleSyntheticRoot()
    const settledTargets = this.#input.unsettledTargets.filter((target) =>
      this.#settledTargetKeys.has(selectionTargetKey(target)))
    return createAuthenticatedProjectionEvidence({
      generations: [Object.freeze({
        directoryId: this.#input.committed.directoryIdText,
        generation: this.#input.committed.generationText,
      })],
      metrics: Object.freeze({
        fileCountLowerBound: this.#selectedFileCount,
        directoryCountLowerBound: this.#selectedDirectoryCount,
        byteCountLowerBound: this.#selectedBytes,
      }),
      ...(this.#representativeFile === undefined
        ? {}
        : { representativeFile: this.#representativeFile }),
      selectedRoots: this.#selectedRoots,
      selectedRootCount: this.#selectedRootCount,
      settledTargets,
      ...(this.#directoryPath.length === 0 && this.#selectedRootCount > 1
        ? { earlyLayoutBasis: Object.freeze({ kind: 'synthetic-selection' as const }) }
        : {}),
    })
  }

  #observeEntry(entry: V2CatalogEntry): void {
    const sourcePath = [...this.#directoryPath, entry.name].join('/')
    settleObservedTarget(
      this.#targetByKey,
      this.#settledTargetKeys,
      entry.kind,
      entry.idText,
      sourcePath,
    )
    if (!this.#input.selection.selected(entry, this.#input.directoryAncestry)) return
    this.#observeSelectedEntry(entry, sourcePath)
    if (this.#input.containingDirectorySelected) return
    this.#selectedRootCount = addCount(this.#selectedRootCount, 1, 'selected root count')
    if (this.#selectedRoots.length < MAX_PROJECTION_SELECTED_ROOT_FACTS) {
      this.#selectedRoots.push(selectedRoot(entry, sourcePath))
    }
  }

  #observeSelectedEntry(entry: V2CatalogEntry, sourcePath: string): void {
    if (entry.kind === 'directory') {
      this.#selectedDirectoryCount = addCount(
        this.#selectedDirectoryCount,
        1,
        'selected directory count',
      )
      return
    }
    this.#selectedFileCount = addCount(this.#selectedFileCount, 1, 'selected file count')
    this.#selectedBytes = addBytes(this.#selectedBytes, entry.expectedSize)
    this.#representativeFile ??= Object.freeze({
      fileId: entry.idText,
      sourcePath,
      portableName: entry.name,
    })
  }

  #settleSyntheticRoot(): void {
    if (this.#directoryPath.length !== 0) return
    for (const target of this.#input.unsettledTargets) {
      if (target.kind === 'synthetic-root' &&
          target.syntheticRoot === this.#input.committed.directoryIdText) {
        this.#settledTargetKeys.add(selectionTargetKey(target))
      }
    }
  }
}

function selectedRoot(entry: V2CatalogEntry, sourcePath: string): SelectedRootFact {
  return Object.freeze(entry.kind === 'file'
    ? { kind: 'file', fileId: entry.idText, sourcePath, portableName: entry.name }
    : { kind: 'directory', directoryId: entry.idText, sourcePath, portableName: entry.name })
}

function settleObservedTarget(
  targets: ReadonlyMap<string, UnsettledSelectionTarget>,
  settled: Set<string>,
  nodeKind: 'directory' | 'file',
  id: string,
  path: string,
): void {
  const nodeKey = selectionTargetKey(Object.freeze({ kind: 'node-id', nodeKind, id }))
  if (targets.has(nodeKey)) settled.add(nodeKey)
  const pathKey = selectionTargetKey(Object.freeze({ kind: 'catalog-path', path }))
  if (targets.has(pathKey)) settled.add(pathKey)
}

function requireCommittedGeneration(committed: V2CommittedDirectory): void {
  if (committed.pageCount <= 0 || committed.entryCount < 0 || committed.omittedCount !== 0n) {
    throw new SelectionProjectionError('projection requires a complete committed catalog generation')
  }
  if (committed.directoryIdText !== encodeBase64Url(committed.directoryId) ||
      committed.generationText !== encodeBase64Url(committed.generation)) {
    throw new SelectionProjectionError('committed generation text does not match authenticated identity')
  }
}

function requireCommittedPage(
  committed: V2CommittedDirectory,
  page: V2CatalogPage,
  expectedPageIndex: number,
): void {
  if (!equalBytes(page.directoryId, committed.directoryId) ||
      !equalBytes(page.generation, committed.generation) ||
      page.pageIndex !== expectedPageIndex ||
      page.terminal !== (expectedPageIndex === committed.pageCount - 1) ||
      page.omittedCount !== (page.terminal ? committed.omittedCount : 0n)) {
    throw new SelectionProjectionError('catalog page does not belong to its committed generation')
  }
}

function requireCompleteReplay(
  committed: V2CommittedDirectory,
  pageCount: number,
  entryCount: number,
  terminalCommitment: Uint8Array<ArrayBuffer> | undefined,
): void {
  if (pageCount !== committed.pageCount || entryCount !== committed.entryCount ||
      terminalCommitment === undefined ||
      !equalBytes(terminalCommitment, committed.terminalCommitment)) {
    throw new SelectionProjectionError('committed generation replay is incomplete')
  }
}

function addCount(current: number, addition: number, label: string): number {
  const next = current + addition
  if (!Number.isSafeInteger(next)) {
    throw new SelectionProjectionError(`${label} exceeds exact integer representation`)
  }
  return next
}

function addBytes(current: bigint, addition: bigint): bigint {
  const next = current + addition
  if (addition < 0n || next > (1n << 64n) - 1n) {
    throw new SelectionProjectionError('selected byte lower bound exceeds its unsigned 64-bit domain')
  }
  return next
}
