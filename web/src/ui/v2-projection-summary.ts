import type { V2CatalogEntry } from '../catalog/v2-records'
import { WorkspaceCostObservationAccumulatorV1 } from '../output/planning/workspace-cost-observation'
import {
  DEFAULT_RESULT_ROOT_NAME,
  createCompleteDirectoryResultRoot,
  createDirectorySelectionResultRoot,
} from '../transfer/intent'
import type {
  AuthenticatedProjectionEvidence,
  SelectedRootFact,
  SettledLayoutBasisProof,
} from '../transfer/projection'

export class ProjectionDiscoverySummary {
  readonly #syntheticRootSelected: boolean
  readonly #partialDirectoryRoots = new Set<string>()
  #selectedFileCount = 0
  #selectedDirectoryCount = 0
  #selectedRootCount = 0
  #singleSelectedRoot: SelectedRootFact | undefined
  readonly #syntheticCost = new WorkspaceCostObservationAccumulatorV1()
  #completeDirectoryCost: WorkspaceCostObservationAccumulatorV1 | undefined
  #partialDirectoryCost: WorkspaceCostObservationAccumulatorV1 | undefined
  #costFailed = false

  constructor(syntheticRootSelected: boolean) {
    this.#syntheticRootSelected = syntheticRootSelected
    this.#observeCost(this.#syntheticCost, {
      kind: 'directory', path: [DEFAULT_RESULT_ROOT_NAME],
    })
  }

  observe(evidence: AuthenticatedProjectionEvidence): void {
    this.#selectedFileCount = Math.min(
      2,
      this.#selectedFileCount + evidence.metrics.fileCountLowerBound,
    )
    this.#selectedDirectoryCount = Math.min(
      1,
      this.#selectedDirectoryCount + evidence.metrics.directoryCountLowerBound,
    )
    if (this.#selectedRootCount === 0 && evidence.selectedRootCount === 1) {
      this.#singleSelectedRoot = evidence.selectedRoots[0]
    }
    this.#selectedRootCount = Math.min(2, this.#selectedRootCount + evidence.selectedRootCount)
    if (this.#selectedRootCount !== 1) this.#singleSelectedRoot = undefined
    const root = this.#singleSelectedRoot
    if (this.#completeDirectoryCost === undefined && root?.kind === 'directory') {
      this.#completeDirectoryCost = new WorkspaceCostObservationAccumulatorV1()
      this.#partialDirectoryCost = new WorkspaceCostObservationAccumulatorV1()
    }
  }

  observeCatalogEntry(
    sourcePath: readonly string[],
    entry: V2CatalogEntry,
    selected: boolean,
  ): void {
    if (!selected || this.#costFailed) return
    this.#observeCost(this.#syntheticCost, costSpec(entry, [DEFAULT_RESULT_ROOT_NAME, ...sourcePath]))
    const root = this.#singleSelectedRoot
    if (root?.kind !== 'directory') return
    const anchor = root.sourcePath.split('/')
    if (!startsWithPath(sourcePath, anchor)) return
    const suffix = sourcePath.slice(anchor.length)
    const completeName = createCompleteDirectoryResultRoot(root.directoryId, root.sourcePath).name
    const partialName = createDirectorySelectionResultRoot(root.directoryId, root.sourcePath).name
    this.#observeCost(this.#completeDirectoryCost!, costSpec(entry, [completeName, ...suffix]))
    this.#observeCost(this.#partialDirectoryCost!, costSpec(entry, [partialName, ...suffix]))
  }

  markDirectoryRootPartial(directoryId: string): void {
    this.#partialDirectoryRoots.add(directoryId)
  }

  layoutBasis(): SettledLayoutBasisProof | undefined {
    const treeRequired = this.#selectedDirectoryCount > 0 || this.#selectedFileCount > 1
    if (!treeRequired) return undefined
    const root = this.#singleSelectedRoot
    if (!this.#syntheticRootSelected && this.#selectedRootCount === 1 && root?.kind === 'directory') {
      return Object.freeze({
        kind: this.#partialDirectoryRoots.has(root.directoryId)
          ? 'directory-selection' as const
          : 'complete-directory' as const,
        anchor: Object.freeze({
          directoryId: root.directoryId,
          sourcePath: root.sourcePath,
        }),
      })
    }
    return Object.freeze({ kind: 'synthetic-selection' as const })
  }

  workspaceCostObservation(layout: SettledLayoutBasisProof | undefined) {
    if (this.#costFailed || layout === undefined) return undefined
    try {
      switch (layout.kind) {
        case 'synthetic-selection': return this.#syntheticCost.complete()
        case 'complete-directory': return this.#completeDirectoryCost?.complete()
        case 'directory-selection': return this.#partialDirectoryCost?.complete()
      }
    } catch {
      // Recommendation evidence is passive; format limits leave the cost unknown.
      return undefined
    }
  }

  #observeCost(
    accumulator: WorkspaceCostObservationAccumulatorV1,
    spec: Parameters<WorkspaceCostObservationAccumulatorV1['observe']>[0],
  ): void {
    try {
      accumulator.observe(spec)
    } catch {
      this.#costFailed = true
    }
  }
}

function costSpec(entry: V2CatalogEntry, path: readonly string[]) {
  return entry.kind === 'file'
    ? Object.freeze({
        kind: 'file' as const,
        path,
        exactSize: entry.expectedSize,
        ...(entry.modifiedTime === undefined
          ? {}
          : { modifiedTimeMilliseconds: entry.modifiedTime.milliseconds }),
      })
    : Object.freeze({
        kind: 'directory' as const,
        path,
        ...(entry.modifiedTime === undefined
          ? {}
          : { modifiedTimeMilliseconds: entry.modifiedTime.milliseconds }),
      })
}

function startsWithPath(path: readonly string[], prefix: readonly string[]): boolean {
  return path.length >= prefix.length && prefix.every((component, index) => path[index] === component)
}
