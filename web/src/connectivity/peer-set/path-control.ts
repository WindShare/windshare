import type { ReceiverSessionRuntime } from '../../session/runtime'
import { equalBytes } from '../../crypto/bytes'
import { decodePeerPathControl, encodePeerPathControl, PEER_PATH_CONTROL_KIND } from '../../protocol/peer-path-control'
import type { PeerRecoveryClock } from './contract'
import type { PeerPathNotice } from './wave'
import type { PeerNetworkGeneration } from './network-generation'
import { awaitWithAbort } from '../peer-offer-remote'

const DEMAND_LIFETIME_MILLISECONDS = 120_000
const DEMAND_RENEWAL_MILLISECONDS = 60_000
const ATTEMPT_HOLD_MILLISECONDS = 85_000

/** Session-authenticated notices outlive negotiation operations without reviving them. */
export class PeerPathControl {
  readonly #session: ReceiverSessionRuntime
  readonly #path: Uint8Array<ArrayBuffer>
  readonly #network: PeerNetworkGeneration
  readonly #clock: PeerRecoveryClock
  readonly #unsubscribe: () => void
  readonly #lifetime = new AbortController()
  readonly #tasks = new Set<Promise<void>>()
  #sequence = 0n
  #remoteSequence = 0n
  #demand: AbortController | undefined
  #closed = false

  constructor(session: ReceiverSessionRuntime, path: Uint8Array, network: PeerNetworkGeneration,
    clock: PeerRecoveryClock, changed: (notice: PeerPathNotice) => void) {
    this.#session = session
    this.#path = path.slice()
    this.#network = network
    this.#clock = clock
    this.#unsubscribe = session.subscribePeerPathControls((body) => {
      const control = decodePeerPathControl(body)
      if (this.#closed || !equalBytes(control.peerPathId, this.#path) ||
        control.controlSequence <= this.#remoteSequence) return
      // Retain the watermark after expiry; neither old network notices nor reclaimed
      // operation records may reintroduce an already consumed mapping-ready hint.
      this.#remoteSequence = control.controlSequence
      if (this.#demand === undefined || control.validForMilliseconds === 0) return
      if (control.kind === PEER_PATH_CONTROL_KIND.mappingReady) changed('mapping-ready')
      if (control.kind === PEER_PATH_CONTROL_KIND.networkChanged) changed('network-changed')
    })
  }

  activate(): void {
    if (this.#closed || this.#demand !== undefined) return
    const demand = new AbortController()
    this.#demand = demand
    this.#track(this.#renew(demand.signal))
  }

  deactivate(): void {
    if (this.#demand === undefined) return
    this.#demand.abort()
    this.#demand = undefined
    // Idle content withdraws its hold; revoke permanently retires this identity
    // at the sender and belongs only to the path owner's final close.
    this.#track(this.#sendDemand())
  }

  async close(): Promise<void> {
    if (!this.#closed) {
      this.#demand?.abort()
      this.#demand = undefined
      if (this.#sequence !== 0n) {
        // Hand final revoke to the session writer without binding #lifetime.signal,
        // so synchronous teardown neither blocks on a stalled relay nor withdraws
        // a queued revoke before it seals.
        void this.#session.sendPeerPathControl(this.#encode(PEER_PATH_CONTROL_KIND.revoke)).catch(() => undefined)
      }
      this.#closed = true
      this.#unsubscribe()
      this.#lifetime.abort()
    }
    await Promise.allSettled([...this.#tasks])
  }

  async #renew(signal: AbortSignal): Promise<void> {
    while (!signal.aborted && !this.#closed) {
      await this.#sendDemand(signal).catch(() => undefined)
      if (signal.aborted || this.#closed) return
      await this.#clock.sleep(DEMAND_RENEWAL_MILLISECONDS, signal)
    }
  }

  #track(task: Promise<void>): void {
    const settled = task.catch(() => undefined).finally(() => this.#tasks.delete(settled))
    this.#tasks.add(settled)
  }

  #sendDemand(signal = this.#lifetime.signal): Promise<void> {
    // Demand cancellation withdraws queued updates and releases this owner's
    // wait so shutdown cannot depend on a stalled relay.
    return awaitWithAbort(
      this.#session.sendPeerPathControl(this.#encode(PEER_PATH_CONTROL_KIND.demand), { signal }),
      signal,
    )
  }

  #encode(kind: number): Uint8Array<ArrayBuffer> {
    const revoke = kind === PEER_PATH_CONTROL_KIND.revoke
    return encodePeerPathControl({
      peerPathId: this.#path, networkGenerationId: this.#network.copyBytes(),
      controlSequence: ++this.#sequence, kind,
      validForMilliseconds: revoke ? 0 : DEMAND_LIFETIME_MILLISECONDS,
      holdForMilliseconds: revoke || this.#demand === undefined ? 0 : ATTEMPT_HOLD_MILLISECONDS,
    })
  }
}
