import type { V2CatalogEntry } from './v2-records'
import { V2_CATALOG_PATH_DEPTH } from './path-policy'

export type V2SelectionState = 'selected' | 'unselected' | 'mixed'

interface SelectionOverride {
  readonly selected: boolean
  readonly ancestry: readonly string[]
}

/**
 * Directory overrides are semantic rules, not snapshots of discovered children.
 * A child discovered later therefore inherits the same decision as one already on screen.
 */
export class V2SelectionPolicy {
  readonly defaultSelected: boolean
  readonly #directoryOverrides = new Map<string, SelectionOverride>()
  readonly #fileOverrides = new Map<string, SelectionOverride>()

  constructor(defaultSelected = true) {
    this.defaultSelected = defaultSelected
  }

  get explicitRuleCount(): number {
    return this.#directoryOverrides.size + this.#fileOverrides.size
  }

  selected(entry: V2CatalogEntry, directoryAncestry: readonly string[]): boolean {
    if (entry.kind === 'file') {
      const file = this.#fileOverrides.get(entry.idText)
      if (file !== undefined) return file.selected
    }
    return this.#selectedByDirectories(
      entry.kind === 'directory'
        ? [...directoryAncestry, entry.idText]
        : directoryAncestry,
    )
  }

  state(entry: V2CatalogEntry, directoryAncestry: readonly string[]): V2SelectionState {
    const selected = this.selected(entry, directoryAncestry)
    if (entry.kind === 'file') return selected ? 'selected' : 'unselected'
    for (const [id, override] of this.#directoryOverrides) {
      if (id !== entry.idText && override.selected !== selected &&
          override.ancestry.includes(entry.idText)) {
        return 'mixed'
      }
    }
    for (const override of this.#fileOverrides.values()) {
      if (override.selected !== selected && override.ancestry.includes(entry.idText)) return 'mixed'
    }
    return selected ? 'selected' : 'unselected'
  }

  toggle(entry: V2CatalogEntry, directoryAncestry: readonly string[]): void {
    const next = !this.selected(entry, directoryAncestry)
    if (entry.kind === 'directory') {
      this.#directoryOverrides.set(entry.idText, Object.freeze({
        selected: next,
        ancestry: snapshotSelectionAncestry([...directoryAncestry, entry.idText]),
      }))
    } else {
      this.#fileOverrides.set(entry.idText, Object.freeze({
        selected: next,
        ancestry: snapshotSelectionAncestry(directoryAncestry),
      }))
    }
  }

  shouldDiscover(directoryId: string, directoryAncestry: readonly string[]): boolean {
    if (this.#selectedByDirectories([...directoryAncestry, directoryId])) return true
    for (const override of this.#directoryOverrides.values()) {
      if (override.selected && override.ancestry.includes(directoryId)) return true
    }
    for (const override of this.#fileOverrides.values()) {
      if (override.selected && override.ancestry.includes(directoryId)) return true
    }
    return false
  }

  #selectedByDirectories(directoryAncestry: readonly string[]): boolean {
    for (let index = directoryAncestry.length - 1; index >= 0; index -= 1) {
      const id = directoryAncestry[index]
      if (id === undefined) continue
      const override = this.#directoryOverrides.get(id)
      if (override !== undefined) return override.selected
    }
    return this.defaultSelected
  }
}

function snapshotSelectionAncestry(ancestry: readonly string[]): readonly string[] {
  if (ancestry.length === 0 || ancestry.length > V2_CATALOG_PATH_DEPTH + 1) {
    throw new RangeError('Catalog selection ancestry exceeds the protocol path depth')
  }
  const identities = new Set(ancestry)
  if (identities.size !== ancestry.length || ancestry.some((identity) => identity.length === 0)) {
    throw new Error('Catalog selection ancestry contains an invalid identity cycle')
  }
  return Object.freeze([...ancestry])
}
