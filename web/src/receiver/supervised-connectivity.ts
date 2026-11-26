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
  unsubscribeRoutes?: () => void
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
    try {
      for (const activation of [...this.#activations.values()]) {
        if (!activation.routes.active) {
          this.#dropActivation(activation)
          continue
        }
        activation.delegate?.close()
        delete activation.delegate
        const delegate = this.#beginDelegate(connectivity, activation)
        if (activation.routes.active) {
          activation.delegate = delegate
        } else {
          delegate.close()
          this.#dropActivation(activation)
        }
      }
    } finally {
      if (previous !== undefined && previous !== connectivity) {
        previous.close().catch(() => undefined)
      }
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
    if (activation.routes.active) {
      this.#activations.set(activation.id, activation)
      // ReceiverConnectivity may close a shared p2p-only authority before the
      // caller finishes unwinding and invokes activation.close().
      activation.unsubscribeRoutes = activation.routes.subscribe(() => {
        if (!activation.routes.active) this.#dropActivation(activation)
      })
    } else {
      activation.delegate?.close()
      delete activation.delegate
    }
    let closed = false
    return Object.freeze({
      routes: activation.routes,
      close: () => {
        if (closed) return
        closed = true
        this.#dropActivation(activation)
        activation.routes.close()
      },
    })
  }

  async close(reason: unknown = new DOMException('Supervised connectivity closed', 'AbortError')): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    for (const activation of [...this.#activations.values()]) {
      this.#dropActivation(activation)
      activation.routes.close(reason)
    }
    this.#activations.clear()
    const current = this.#current
    this.#current = undefined
    await current?.close()
  }

  #dropActivation(activation: StableActivation): void {
    this.#activations.delete(activation.id)
    activation.unsubscribeRoutes?.()
    delete activation.unsubscribeRoutes
    activation.delegate?.close()
    delete activation.delegate
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
