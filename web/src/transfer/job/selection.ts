import type { V2CatalogEntry } from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { encodeBase64Url } from '../../crypto/bytes'

export interface MissingV2SelectionTarget {
  readonly kind: 'directory' | 'file'
  readonly idText: string
}

/** Tracks only selected opaque targets; unselected overrides require no settlement. */
export class V2ExplicitSelectionTargetLedger {
  readonly #pending: Map<string, MissingV2SelectionTarget>
  readonly #lifetime: AbortSignal
  readonly #opaqueSearch = new AbortController()

  constructor(selection: V2FrozenSelectionPolicy, lifetime: AbortSignal) {
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

  observe(entry: V2CatalogEntry): void {
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

  missing(): readonly MissingV2SelectionTarget[] {
    return Object.freeze([...this.#pending.values()])
  }

  #stopSatisfiedSearch(): void {
    if (!this.hasPendingTargets && !this.#opaqueSearch.signal.aborted) {
      this.#opaqueSearch.abort(new Error('All selected opaque catalog targets were authenticated'))
    }
  }
}

export class V2SelectionTargetMissingError extends Error {
  constructor(target: MissingV2SelectionTarget) {
    super(`Selected ${target.kind} target was not found in terminal catalog discovery`)
    this.name = 'V2SelectionTargetMissingError'
  }
}

function targetKey(kind: MissingV2SelectionTarget['kind'], idText: string): string {
  return `${kind}:${idText}`
}
