import {
  V2_RELAY_CONTENT_FALLBACK_MILLISECONDS,
  type V2ConnectivityActivation,
  V2ConnectivityRouteAuthority,
  type V2ContentIntent,
  type V2ContentSizeClass,
  V2ReceiverConnectivity,
} from '../connectivity/v2-receiver-policy'

interface V2StableActivation {
  readonly id: number
  readonly intent: V2ContentIntent
  readonly startedAt: number
  readonly routes: V2ConnectivityRouteAuthority
  sizeClass: V2ContentSizeClass
  delegate?: V2ConnectivityActivation
}

/** Keeps click-time 0/8 policy and activation ownership above session generations. */
export class V2SupervisedConnectivity {
  readonly #now: () => number
  readonly #activations = new Map<number, V2StableActivation>()
  #current: V2ReceiverConnectivity | undefined
  #nextActivation = 1
  #closed = false

  constructor(now: () => number = () => performance.now()) {
    this.#now = now
  }

  bind(connectivity: V2ReceiverConnectivity): void {
    if (this.#closed) {
      connectivity.close().catch(() => undefined)
      return
    }
    const previous = this.#current
    this.#current = connectivity
    for (const activation of this.#activations.values()) {
      activation.delegate?.close()
      activation.delegate = this.#beginDelegate(connectivity, activation)
    }
    if (previous !== undefined && previous !== connectivity) {
      previous.close().catch(() => undefined)
    }
  }

  begin(
    intent: V2ContentIntent,
    sizeClass: V2ContentSizeClass = 'unknown',
  ): V2ConnectivityActivation {
    if (this.#closed) throw new Error('Supervised connectivity is closed')
    const activation: V2StableActivation = {
      id: this.#nextActivation++,
      intent,
      sizeClass,
      startedAt: this.#now(),
      routes: new V2ConnectivityRouteAuthority(),
    }
    if (this.#current !== undefined) {
      activation.delegate = this.#beginDelegate(this.#current, activation)
    }
    this.#activations.set(activation.id, activation)
    let closed = false
    return Object.freeze({
      routes: activation.routes,
      observeSizeClass: (observed: V2ContentSizeClass) => {
        if (closed) return
        activation.sizeClass = observed
        activation.delegate?.observeSizeClass(observed)
      },
      close: () => {
        if (closed) return
        closed = true
        this.#activations.delete(activation.id)
        activation.delegate?.close()
        activation.routes.close()
        delete activation.delegate
      },
    })
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    for (const activation of this.#activations.values()) {
      activation.delegate?.close()
      activation.routes.close(new DOMException('Supervised connectivity closed', 'AbortError'))
    }
    this.#activations.clear()
    const current = this.#current
    this.#current = undefined
    await current?.close()
  }

  #beginDelegate(
    connectivity: V2ReceiverConnectivity,
    activation: V2StableActivation,
  ): V2ConnectivityActivation {
    const elapsed = Math.max(0, this.#now() - activation.startedAt)
    const remaining = Math.max(0, V2_RELAY_CONTENT_FALLBACK_MILLISECONDS - elapsed)
    return connectivity.begin(activation.intent, activation.sizeClass, {
      relayFallbackMilliseconds: remaining,
      routeAuthority: activation.routes,
    })
  }
}
