import type { PeerPathRoute } from '../peer-channel'

interface SelectedPairTransport extends Pick<EventTarget, 'addEventListener' | 'removeEventListener'> {
  getSelectedCandidatePair?(): {
    readonly local: { readonly type: unknown }
    readonly remote: { readonly type: unknown }
  } | null
}

const CANDIDATE_TYPES: ReadonlySet<unknown> = new Set(['host', 'srflx', 'prflx', 'relay'])

/** The selected transport pair owns routing; statistics are asynchronous observations. */
export class BrowserPeerRoute {
  readonly #readTransport: () => SelectedPairTransport | undefined
  readonly #changed: (route: PeerPathRoute) => void
  #transport: SelectedPairTransport | undefined
  #current: PeerPathRoute
  #closed = false

  constructor(
    readTransport: () => SelectedPairTransport | undefined,
    changed: (route: PeerPathRoute) => void,
  ) {
    this.#readTransport = readTransport
    this.#changed = changed
  }

  get current(): PeerPathRoute { return this.#current }

  refresh = (): void => {
    if (this.#closed) return
    const transport = this.#readTransport()
    if (transport !== this.#transport) {
      this.#unsubscribe()
      this.#transport = transport
      transport?.addEventListener('selectedcandidatepairchange', this.refresh)
      transport?.addEventListener('statechange', this.refresh)
    }
    if (typeof transport?.getSelectedCandidatePair !== 'function') return
    try {
      const pair = transport.getSelectedCandidatePair()
      this.#set(pair === null ? undefined : classifyPair(pair.local.type, pair.remote.type))
    } catch {
      this.#set(undefined)
    }
  }

  observeStats(route: PeerPathRoute): void {
    if (this.#closed) return
    this.refresh()
    // Some supported providers expose the pair only through stats. An absent
    // snapshot does not prove transport loss and must not revoke a known route.
    if (!this.#hasNativePair() && route !== undefined) this.#set(route)
  }

  close(): void {
    this.#closed = true
    this.#unsubscribe()
    this.#transport = undefined
  }

  #hasNativePair(): boolean {
    return typeof this.#transport?.getSelectedCandidatePair === 'function'
  }

  #unsubscribe(): void {
    this.#transport?.removeEventListener('selectedcandidatepairchange', this.refresh)
    this.#transport?.removeEventListener('statechange', this.refresh)
  }

  #set(route: PeerPathRoute): void {
    if (route === this.#current) return
    this.#current = route
    this.#changed(route)
  }
}

function classifyPair(local: unknown, remote: unknown): PeerPathRoute {
  if (!CANDIDATE_TYPES.has(local) || !CANDIDATE_TYPES.has(remote)) return undefined
  return local === 'relay' || remote === 'relay' ? 'turn' : 'direct'
}
