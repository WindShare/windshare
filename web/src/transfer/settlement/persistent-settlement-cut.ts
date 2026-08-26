import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type {
  PersistentMaterializationEvidence,
  PersistentMaterializationSettlementCut,
} from './persistent-evidence'

export class PersistentSettlementCut<Evidence extends PersistentMaterializationEvidence>
implements PersistentMaterializationSettlementCut<Evidence> {
  readonly #evidence: Evidence | (() => Evidence)
  readonly #close: () => Promise<void>
  #snapshot: Evidence | undefined
  #sealed = false
  #closePromise: Promise<void> | undefined

  constructor(evidence: Evidence | (() => Evidence), close: () => Promise<void>) {
    this.#evidence = evidence
    this.#close = close
  }

  get evidence(): Evidence {
    return this.snapshotQuiescentEvidence()
  }

  snapshotQuiescentEvidence(): Evidence {
    this.#snapshot ??= typeof this.#evidence === 'function'
      ? (this.#evidence as () => Evidence)()
      : this.#evidence
    return this.#snapshot
  }

  sealEvidence(): Evidence {
    const evidence = this.snapshotQuiescentEvidence()
    this.#sealed = true
    return evidence
  }

  closeMaterialization(): Promise<void> {
    this.#closePromise ??= this.#close()
    return this.#closePromise
  }

  async validateReturnedState(state: ReceiveLifecycleState): Promise<void> {
    if (this.#closePromise === undefined) {
      throw new TypeError('persistent lifecycle settlement returned before closing materialization')
    }
    if (!this.#sealed) this.sealEvidence()
    try {
      await this.#closePromise
    } catch (cause) {
      if (state.kind !== 'needs-attention') {
        throw new TypeError(
          'persistent lifecycle settlement hid materialization close uncertainty',
          { cause },
        )
      }
    }
  }
}
