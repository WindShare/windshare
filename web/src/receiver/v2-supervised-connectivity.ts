import {
  type V2ConnectivityActivation,
  V2ConnectivityRouteAuthority,
  type V2ContentIntent,
  V2ReceiverConnectivity,
  type V2ConnectivityPolicy,
} from '../connectivity/v2-receiver-policy'

interface V2StableActivation {
  readonly id: number
  readonly intent: V2ContentIntent
  readonly routes: V2ConnectivityRouteAuthority
  delegate?: V2ConnectivityActivation
}

/** Keeps click-scoped route authority stable while ProtocolSession generations change. */
export class V2SupervisedConnectivity {
  readonly #policy: V2ConnectivityPolicy
  readonly #activations = new Map<number, V2StableActivation>()
  #current: V2ReceiverConnectivity | undefined
  #nextActivation = 1
  #closed = false

  constructor(policy: V2ConnectivityPolicy = 'auto') { this.#policy = policy }

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

  begin(intent: V2ContentIntent): V2ConnectivityActivation {
    if (this.#closed) throw new Error('Supervised connectivity is closed')
    const activation: V2StableActivation = {
      id: this.#nextActivation++,
      intent,
      routes: new V2ConnectivityRouteAuthority(this.#policy),
    }
    if (this.#current !== undefined) {
      activation.delegate = this.#beginDelegate(this.#current, activation)
    }
    this.#activations.set(activation.id, activation)
    let closed = false
    return Object.freeze({
      routes: activation.routes,
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
    return connectivity.begin(activation.intent, {
      routeAuthority: activation.routes,
    })
  }
}
