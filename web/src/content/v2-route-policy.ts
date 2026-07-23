export type V2BlockTransportRoute = 'relay' | 'peer'

/** Per-consumer authority for starting new upstream content work on a route. */
export interface V2BlockRouteEligibility {
  readonly active: boolean
  allows(route: V2BlockTransportRoute): boolean
  assertActive(): void
  subscribe(listener: () => void): () => void
}

interface SharedRouteConsumer {
  count: number
  readonly unsubscribe: () => void
}

/** Union authority used only by one coalesced BlockRef load. */
export class SharedV2BlockRouteEligibility implements V2BlockRouteEligibility {
  readonly #consumers = new Map<V2BlockRouteEligibility, SharedRouteConsumer>()
  readonly #listeners = new Set<() => void>()

  get active(): boolean {
    return [...this.#consumers.keys()].some((routes) => routes.active)
  }

  allows(route: V2BlockTransportRoute): boolean {
    return [...this.#consumers.keys()].some((routes) => routes.active && routes.allows(route))
  }

  assertActive(): void {
    if (this.active) return
    const first = this.#consumers.keys().next().value as V2BlockRouteEligibility | undefined
    if (first !== undefined) first.assertActive()
    throw new DOMException('Block route eligibility has no active consumer', 'AbortError')
  }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  add(routes: V2BlockRouteEligibility): () => void {
    routes.assertActive()
    const existing = this.#consumers.get(routes)
    if (existing !== undefined) {
      existing.count += 1
    } else {
      const unsubscribe = routes.subscribe(() => this.#notify())
      this.#consumers.set(routes, { count: 1, unsubscribe })
    }
    this.#notify()
    let released = false
    return () => {
      if (released) return
      released = true
      const consumer = this.#consumers.get(routes)
      if (consumer === undefined) return
      consumer.count -= 1
      if (consumer.count === 0) {
        consumer.unsubscribe()
        this.#consumers.delete(routes)
      }
      this.#notify()
    }
  }

  #notify(): void {
    for (const listener of this.#listeners) {
      try {
        listener()
      } catch {
        // A waiter owns its own failure boundary; notification is only a wakeup.
      }
    }
  }
}
