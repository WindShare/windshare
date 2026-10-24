import {
  type ConnectivityActivation,
  ConnectivityRouteAuthority,
  type ContentIntent,
  ReceiverConnectivity,
  type ConnectivityPolicy,
} from '../connectivity/receiver-policy'

interface StableActivation {
  readonly id: number
  readonly intent: ContentIntent
  readonly routes: ConnectivityRouteAuthority
  delegate?: ConnectivityActivation
}

/** Keeps click-scoped route authority stable while ProtocolSession generations change. */
export class SupervisedConnectivity {
  readonly #policy: ConnectivityPolicy
  readonly #activations = new Map<number, StableActivation>()
  #current: ReceiverConnectivity | undefined
  #nextActivation = 1
  #closed = false

  constructor(policy: ConnectivityPolicy = 'auto') { this.#policy = policy }

  bind(connectivity: ReceiverConnectivity): void {
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

  begin(intent: ContentIntent): ConnectivityActivation {
    if (this.#closed) throw new Error('Supervised connectivity is closed')
    const activation: StableActivation = {
      id: this.#nextActivation++,
      intent,
      routes: new ConnectivityRouteAuthority(this.#policy),
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
    connectivity: ReceiverConnectivity,
    activation: StableActivation,
  ): ConnectivityActivation {
    return connectivity.begin(activation.intent, {
      routeAuthority: activation.routes,
    })
  }
}
