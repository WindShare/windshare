import type { ReceiveLifecycleState } from '../state'
import { canonicalReceiveLifecycleStateBytes } from '../state-codec'
import { equalBytes } from '../../../crypto/bytes'

export function sameReceiveLifecycleState(left: ReceiveLifecycleState, right: ReceiveLifecycleState): boolean {
  return left === right || (left.timing?.startedAtMilliseconds === right.timing?.startedAtMilliseconds &&
    left.timing?.resultReadyAtMilliseconds === right.timing?.resultReadyAtMilliseconds &&
    equalBytes(canonicalReceiveLifecycleStateBytes(left), canonicalReceiveLifecycleStateBytes(right)))
}

export type ReceiveLifecycleListener = (state: ReceiveLifecycleState) => void

/** Notifications carry committed facts; observers cannot change a commit's outcome. */
export class ReceiveLifecycleNotifications {
  readonly #listeners = new Set<ReceiveLifecycleListener>()

  subscribe(listener: ReceiveLifecycleListener): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  publish(state: ReceiveLifecycleState): void {
    for (const listener of this.#listeners) {
      try { listener(state) } catch {
        // Presentation failures must not counterfeit a failed durable transition.
      }
    }
  }

  close(): void { this.#listeners.clear() }
}

/** One operation's current snapshot, independent of transfer or writer settlement. */
export class ReceiveLifecycleObservation {
  readonly #notifications = new ReceiveLifecycleNotifications()
  #state: ReceiveLifecycleState

  constructor(initial: ReceiveLifecycleState) { this.#state = initial }

  getSnapshot(): ReceiveLifecycleState { return this.#state }

  subscribe(listener: ReceiveLifecycleListener): () => void {
    return this.#notifications.subscribe(listener)
  }

  publish(state: ReceiveLifecycleState): void {
    if (state.operationId !== this.#state.operationId ||
        state.receiveIntentDigest !== this.#state.receiveIntentDigest ||
        state.generation <= this.#state.generation) return
    this.#state = state
    this.#notifications.publish(state)
  }

  close(): void { this.#notifications.close() }
}
