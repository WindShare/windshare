import type { V2CatalogEntry } from './v2-records'
import { V2_CATALOG_PATH_DEPTH } from './path-policy'

export type V2SelectionState = 'selected' | 'unselected' | 'mixed'
export type V2SelectionDecision = 'explicit-node-rule' | 'ancestor-directory-rule' | 'default-rule'

interface SelectionOverride {
  readonly selected: boolean
  readonly ancestry: readonly string[]
  readonly id: Uint8Array<ArrayBuffer>
}

export const V2_MAXIMUM_SELECTION_RULE_OVERRIDES = 4_096

export interface V2CanonicalSelectionRule {
  readonly kind: 'directory' | 'file'
  readonly id: Uint8Array<ArrayBuffer>
  readonly selected: boolean
}

export interface V2FrozenSelectionPolicy {
  readonly defaultSelected: boolean
  readonly canonicalRules: readonly V2CanonicalSelectionRule[]
  selected(entry: V2CatalogEntry, directoryAncestry: readonly string[]): boolean
  directorySelected(directoryId: string, directoryAncestry: readonly string[]): boolean
  decision(entry: V2CatalogEntry, directoryAncestry: readonly string[]): V2SelectionDecision
  shouldDiscover(directoryId: string, directoryAncestry: readonly string[]): boolean
}

/**
 * Directory overrides are semantic rules, not snapshots of discovered children.
 * A child discovered later therefore inherits the same decision as one already on screen.
 */
class SelectionPolicyEvaluator {
  readonly defaultSelected: boolean
  protected readonly directoryOverrides: Map<string, SelectionOverride>
  protected readonly fileOverrides: Map<string, SelectionOverride>
  protected selectedOverrideCount: number

  constructor(
    defaultSelected: boolean,
    directoryOverrides: Map<string, SelectionOverride>,
    fileOverrides: Map<string, SelectionOverride>,
  ) {
    this.defaultSelected = defaultSelected
    this.directoryOverrides = directoryOverrides
    this.fileOverrides = fileOverrides
    this.selectedOverrideCount = [...directoryOverrides.values(), ...fileOverrides.values()]
      .filter((override) => override.selected).length
  }

  selected(entry: V2CatalogEntry, directoryAncestry: readonly string[]): boolean {
    if (entry.kind === 'file') {
      const file = this.fileOverrides.get(entry.idText)
      if (file !== undefined) return file.selected
    }
    return entry.kind === 'directory'
      ? this.directorySelected(entry.idText, directoryAncestry)
      : this.selectedByDirectories(directoryAncestry)
  }

  directorySelected(directoryId: string, directoryAncestry: readonly string[]): boolean {
    return this.selectedByDirectories([...directoryAncestry, directoryId])
  }

  decision(entry: V2CatalogEntry, directoryAncestry: readonly string[]): V2SelectionDecision {
    if ((entry.kind === 'file' && this.fileOverrides.has(entry.idText)) ||
        (entry.kind === 'directory' && this.directoryOverrides.has(entry.idText))) {
      return 'explicit-node-rule'
    }
    for (let index = directoryAncestry.length - 1; index >= 0; index -= 1) {
      const identity = directoryAncestry[index]
      if (identity !== undefined && this.directoryOverrides.has(identity)) {
        return 'ancestor-directory-rule'
      }
    }
    return 'default-rule'
  }

  shouldDiscover(directoryId: string, directoryAncestry: readonly string[]): boolean {
    if (this.directorySelected(directoryId, directoryAncestry)) return true
    // Opaque node identities cannot authenticate a caller-captured ancestry
    // chain. Keep that chain for UI presentation, but never use it to prune a
    // selected target that may live below this directory.
    return this.selectedOverrideCount > 0
  }

  protected selectedByDirectories(directoryAncestry: readonly string[]): boolean {
    for (let index = directoryAncestry.length - 1; index >= 0; index -= 1) {
      const id = directoryAncestry[index]
      if (id === undefined) continue
      const override = this.directoryOverrides.get(id)
      if (override !== undefined) return override.selected
    }
    return this.defaultSelected
  }
}

export class V2SelectionPolicy extends SelectionPolicyEvaluator {

  constructor(defaultSelected = true) {
    super(defaultSelected, new Map(), new Map())
  }

  get explicitRuleCount(): number {
    return this.directoryOverrides.size + this.fileOverrides.size
  }

  state(entry: V2CatalogEntry, directoryAncestry: readonly string[]): V2SelectionState {
    const selected = this.selected(entry, directoryAncestry)
    if (entry.kind === 'file') return selected ? 'selected' : 'unselected'
    for (const [id, override] of this.directoryOverrides) {
      if (id !== entry.idText && override.selected !== selected &&
          override.ancestry.includes(entry.idText)) {
        return 'mixed'
      }
    }
    for (const override of this.fileOverrides.values()) {
      if (override.selected !== selected && override.ancestry.includes(entry.idText)) return 'mixed'
    }
    return selected ? 'selected' : 'unselected'
  }

  toggle(entry: V2CatalogEntry, directoryAncestry: readonly string[]): void {
    const next = !this.selected(entry, directoryAncestry)
    const existing = entry.kind === 'directory'
      ? this.directoryOverrides.has(entry.idText)
      : this.fileOverrides.has(entry.idText)
    if (!existing && this.explicitRuleCount >= V2_MAXIMUM_SELECTION_RULE_OVERRIDES) {
      throw new RangeError('Catalog selection rule count exceeds the protocol limit')
    }
    if (entry.kind === 'directory') {
      this.replaceOverride(this.directoryOverrides, entry.idText, Object.freeze({
        selected: next,
        ancestry: snapshotSelectionAncestry([...directoryAncestry, entry.idText]),
        id: entry.id.slice(),
      }))
    } else {
      this.replaceOverride(this.fileOverrides, entry.idText, Object.freeze({
        selected: next,
        ancestry: snapshotSelectionAncestry(directoryAncestry),
        id: entry.id.slice(),
      }))
    }
  }

  private replaceOverride(
    overrides: Map<string, SelectionOverride>,
    identity: string,
    replacement: SelectionOverride,
  ): void {
    const previous = overrides.get(identity)
    if (previous?.selected === true) this.selectedOverrideCount -= 1
    if (replacement.selected) this.selectedOverrideCount += 1
    overrides.set(identity, replacement)
  }

  snapshot(): V2FrozenSelectionPolicy {
    return new FrozenV2SelectionPolicy(
      this.defaultSelected,
      new Map(this.directoryOverrides),
      new Map(this.fileOverrides),
    )
  }
}

class FrozenV2SelectionPolicy extends SelectionPolicyEvaluator implements V2FrozenSelectionPolicy {
  readonly canonicalRules: readonly V2CanonicalSelectionRule[]

  constructor(
    defaultSelected: boolean,
    directories: ReadonlyMap<string, SelectionOverride>,
    files: ReadonlyMap<string, SelectionOverride>,
  ) {
    const frozenDirectories = snapshotOverrides(directories)
    const frozenFiles = snapshotOverrides(files)
    super(defaultSelected, frozenDirectories, frozenFiles)
    this.canonicalRules = Object.freeze([
      ...[...frozenDirectories.values()].map((override) => Object.freeze({
        kind: 'directory' as const,
        id: override.id.slice(),
        selected: override.selected,
      })),
      ...[...frozenFiles.values()].map((override) => Object.freeze({
        kind: 'file' as const,
        id: override.id.slice(),
        selected: override.selected,
      })),
    ])
  }

}

function snapshotOverrides(
  source: ReadonlyMap<string, SelectionOverride>,
): Map<string, SelectionOverride> {
  return new Map([...source].map(([identity, override]) => [identity, Object.freeze({
    selected: override.selected,
    ancestry: Object.freeze([...override.ancestry]),
    id: override.id.slice(),
  })]))
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
