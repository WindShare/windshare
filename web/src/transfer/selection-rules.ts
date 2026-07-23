import type {
  CatalogNode,
  CatalogNodeId,
  DirectoryId,
  FileId,
} from '../catalog/model'
import { ProgressiveCatalogTree } from '../catalog/tree'

export type SelectionState = 'selected' | 'unselected' | 'mixed'

/** Immutable rules remain meaningful before any descendant has been discovered. */
export class SelectionRules {
  readonly defaultSelected: boolean

  readonly #directoryOverrides: ReadonlyMap<DirectoryId, boolean>
  readonly #fileOverrides: ReadonlyMap<FileId, boolean>

  constructor(
    defaultSelected: boolean,
    directoryOverrides: ReadonlyMap<DirectoryId, boolean> = new Map(),
    fileOverrides: ReadonlyMap<FileId, boolean> = new Map(),
  ) {
    this.defaultSelected = defaultSelected
    this.#directoryOverrides = new Map(directoryOverrides)
    this.#fileOverrides = new Map(fileOverrides)
  }

  withDirectory(id: DirectoryId, selected: boolean): SelectionRules {
    const overrides = new Map(this.#directoryOverrides)
    overrides.set(id, selected)
    return new SelectionRules(this.defaultSelected, overrides, this.#fileOverrides)
  }

  withoutDirectory(id: DirectoryId): SelectionRules {
    const overrides = new Map(this.#directoryOverrides)
    overrides.delete(id)
    return new SelectionRules(this.defaultSelected, overrides, this.#fileOverrides)
  }

  withFile(id: FileId, selected: boolean): SelectionRules {
    const overrides = new Map(this.#fileOverrides)
    overrides.set(id, selected)
    return new SelectionRules(this.defaultSelected, this.#directoryOverrides, overrides)
  }

  withoutFile(id: FileId): SelectionRules {
    const overrides = new Map(this.#fileOverrides)
    overrides.delete(id)
    return new SelectionRules(this.defaultSelected, this.#directoryOverrides, overrides)
  }

  selected(tree: ProgressiveCatalogTree, id: CatalogNodeId): boolean {
    const node = tree.requireNode(id)
    if (node.kind === 'file') {
      const fileRule = this.#fileOverrides.get(node.id)
      if (fileRule !== undefined) {
        return fileRule
      }
    }
    return this.#selectedByAncestry(tree.ancestry(id))
  }

  /** Resolves a not-yet-created child from its known directory lineage. */
  inheritedBy(ancestors: readonly DirectoryId[]): boolean {
    for (let index = ancestors.length - 1; index >= 0; index -= 1) {
      const ancestor = ancestors[index]
      if (ancestor === undefined) {
        continue
      }
      const override = this.#directoryOverrides.get(ancestor)
      if (override !== undefined) {
        return override
      }
    }
    return this.defaultSelected
  }

  state(tree: ProgressiveCatalogTree, id: CatalogNodeId): SelectionState {
    const selected = this.selected(tree, id)
    const node = tree.requireNode(id)
    if (node.kind === 'file') {
      return selected ? 'selected' : 'unselected'
    }
    for (const [descendant, override] of this.#fileOverrides) {
      if (tree.node(descendant) !== undefined &&
          tree.isDescendant(descendant, node.id) &&
          override !== selected) {
        return 'mixed'
      }
    }
    for (const [descendant, override] of this.#directoryOverrides) {
      if (descendant !== node.id &&
          tree.node(descendant) !== undefined &&
          tree.isDescendant(descendant, node.id) &&
          override !== selected) {
        return 'mixed'
      }
    }
    return selected ? 'selected' : 'unselected'
  }

  /**
   * An unselected directory still needs discovery when a known descendant rule
   * overrides it. Unknown descendants cannot have a rule and inherit `false`.
   */
  shouldDiscoverDirectory(tree: ProgressiveCatalogTree, id: DirectoryId): boolean {
    if (this.selected(tree, id)) {
      return true
    }
    for (const [descendant, selected] of this.#directoryOverrides) {
      if (selected && descendant !== id && tree.node(descendant) !== undefined &&
          tree.isDescendant(descendant, id)) {
        return true
      }
    }
    for (const [descendant, selected] of this.#fileOverrides) {
      if (selected && tree.node(descendant) !== undefined &&
          tree.isDescendant(descendant, id)) {
        return true
      }
    }
    return false
  }

  #selectedByAncestry(ancestry: readonly CatalogNode[]): boolean {
    for (let index = ancestry.length - 1; index >= 0; index -= 1) {
      const node = ancestry[index]
      if (node?.kind !== 'directory') {
        continue
      }
      const override = this.#directoryOverrides.get(node.id)
      if (override !== undefined) {
        return override
      }
    }
    return this.defaultSelected
  }
}
