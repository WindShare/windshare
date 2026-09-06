import type { V2CatalogEntry } from '../../catalog/v2-records'
import type { V2CanonicalSelectionRule, V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { encodeBase64Url } from '../../crypto/bytes'

interface RuleTarget {
  readonly rule: V2CanonicalSelectionRule
  readonly ancestry?: readonly string[]
}

/** Hints bound draft discovery only after every traversed edge has been authenticated. */
export class ProjectionDemand {
  readonly #remaining = new Map<string, RuleTarget>()

  constructor(selection: V2FrozenSelectionPolicy) {
    for (const rule of selection.canonicalRules) {
      const id = encodeBase64Url(rule.id)
      const hint = selection.draftRules?.find(candidate =>
        candidate.kind === rule.kind && encodeBase64Url(candidate.id) === id)
      const ancestry = hint?.ancestry.filter(ancestor => ancestor !== id)
      this.#remaining.set(id, {
        rule,
        ...(ancestry === undefined || ancestry.length === 0 ? {} : { ancestry }),
      })
    }
  }

  get settled(): boolean { return this.#remaining.size === 0 }

  observe(entry: V2CatalogEntry, ancestry: readonly string[]): void {
    const target = this.#remaining.get(entry.idText)
    if (target === undefined) return
    if (target.rule.kind !== entry.kind || (target.ancestry !== undefined &&
      !samePath(target.ancestry, ancestry))) {
      throw new TypeError('Selection path hint disagrees with authenticated catalog ancestry')
    }
    this.#remaining.delete(entry.idText)
  }

  needsDirectory(ancestry: readonly string[]): boolean {
    for (const target of this.#remaining.values()) {
      if (target.ancestry === undefined || isPrefix(ancestry, target.ancestry)) return true
    }
    return false
  }
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && isPrefix(left, right)
}

function isPrefix(prefix: readonly string[], path: readonly string[]): boolean {
  return prefix.length <= path.length && prefix.every((part, index) => part === path[index])
}
