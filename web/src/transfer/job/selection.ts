import type { CatalogEntry } from '../../catalog/records'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import { encodeBase64Url } from '../../crypto/bytes'

export interface MissingSelectionTarget {
  readonly kind: 'directory' | 'file'
  readonly idText: string
}

/** Tracks only selected opaque targets; unselected overrides require no settlement. */
export class ExplicitSelectionTargetLedger {
  readonly #pending: Map<string, MissingSelectionTarget>
  readonly #lifetime: AbortSignal
  readonly #opaqueSearch = new AbortController()

  constructor(selection: FrozenSelectionPolicy, lifetime: AbortSignal) {
    this.#lifetime = lifetime
    this.#pending = new Map(selection.canonicalRules
      .filter((rule) => rule.selected)
      .map((rule) => {
        const target = Object.freeze({ kind: rule.kind, idText: encodeBase64Url(rule.id) })
        return [targetKey(target.kind, target.idText), target]
      }))
    lifetime.addEventListener('abort', () => {
      this.#opaqueSearch.abort(lifetime.reason)
    }, { once: true })
    this.#stopSatisfiedSearch()
  }

  observe(entry: CatalogEntry): void {
    this.#pending.delete(targetKey(entry.kind, encodeBase64Url(entry.id)))
    this.#stopSatisfiedSearch()
  }

  observeDirectory(id: Uint8Array): void {
    this.#pending.delete(targetKey('directory', encodeBase64Url(id)))
    this.#stopSatisfiedSearch()
  }

  get hasPendingTargets(): boolean { return this.#pending.size > 0 }

  discoverySignal(selectedDirectory: boolean): AbortSignal {
    return selectedDirectory ? this.#lifetime : this.#opaqueSearch.signal
  }

  opaqueSearchSatisfied(selectedDirectory: boolean): boolean {
    return !selectedDirectory && !this.hasPendingTargets && !this.#lifetime.aborted
  }

  missing(): readonly MissingSelectionTarget[] {
    return Object.freeze([...this.#pending.values()])
  }

  #stopSatisfiedSearch(): void {
    if (!this.hasPendingTargets && !this.#opaqueSearch.signal.aborted) {
      this.#opaqueSearch.abort(new Error('All selected opaque catalog targets were authenticated'))
    }
  }
}

export class SelectionTargetMissingError extends Error {
  constructor(target: MissingSelectionTarget) {
    super(`Selected ${target.kind} target was not found in terminal catalog discovery`)
    this.name = 'SelectionTargetMissingError'
  }
}

function targetKey(kind: MissingSelectionTarget['kind'], idText: string): string {
  return `${kind}:${idText}`
}
