import type { V2ReceiverSessionRuntime } from '../../session/v2-runtime'
import { equalBytes } from '../../crypto/bytes'
import { decodeV2PeerPathControl, encodeV2PeerPathControl, V2_PEER_PATH_CONTROL_KIND } from '../v2-path-control-codec'
import type { V2PeerRecoveryClock } from './contract'
import type { PeerPathNotice } from './wave'
import type { PeerNetworkGeneration } from './network-generation'
import { awaitWithAbort } from '../peer-offer-remote'

const DEMAND_LIFETIME_MILLISECONDS = 120_000
const DEMAND_RENEWAL_MILLISECONDS = 60_000
const ATTEMPT_HOLD_MILLISECONDS = 85_000

/** Session-authenticated notices outlive negotiation operations without reviving them. */
export class PeerPathControl {
  readonly #session: V2ReceiverSessionRuntime
  readonly #path: Uint8Array<ArrayBuffer>
  readonly #network: PeerNetworkGeneration
  readonly #clock: V2PeerRecoveryClock
  readonly #unsubscribe: () => void
  readonly #providerProfile: string
  readonly #lifetime = new AbortController()
  readonly #tasks = new Set<Promise<void>>()
  #remoteProviderProfile = ''
  #sequence = 0n
  #remoteSequence = 0n
  #demand: AbortController | undefined
  #closed = false

  constructor(session: V2ReceiverSessionRuntime, path: Uint8Array, network: PeerNetworkGeneration,
    clock: V2PeerRecoveryClock, changed: (notice: PeerPathNotice) => void, providerProfile = '') {
    this.#session = session
    this.#path = path.slice()
    this.#network = network
    this.#clock = clock
    if (!/^[a-z0-9.-]{0,64}$/.test(providerProfile)) throw new TypeError('Invalid trusted provider profile')
    this.#providerProfile = providerProfile
    this.#unsubscribe = session.subscribePeerPathControls((body) => {
      const control = decodeV2PeerPathControl(body)
      if (this.#closed || !equalBytes(control.peerPathId, this.#path) ||
        control.controlSequence <= this.#remoteSequence) return
      // Retain the watermark after expiry; neither old network notices nor reclaimed
      // operation records may reintroduce an already consumed mapping-ready hint.
      this.#remoteSequence = control.controlSequence
      this.#remoteProviderProfile = control.providerProfile ?? ''
      if (this.#demand === undefined || control.validForMilliseconds === 0) return
      if (control.kind === V2_PEER_PATH_CONTROL_KIND.mappingReady) changed('mapping-ready')
      if (control.kind === V2_PEER_PATH_CONTROL_KIND.networkChanged) changed('network-changed')
    })
  }

  activate(): void {
    if (this.#closed || this.#demand !== undefined) return
    const demand = new AbortController()
    this.#demand = demand
    this.#track(this.#renew(demand.signal))
  }

  get remoteProviderProfile(): string { return this.#remoteProviderProfile }

  revoke(): void {
    if (this.#demand === undefined) return
    this.#demand.abort()
    this.#demand = undefined
    this.#track(this.#send(V2_PEER_PATH_CONTROL_KIND.revoke))
  }

  async close(): Promise<void> {
    if (!this.#closed) {
      this.revoke()
      this.#closed = true
      this.#unsubscribe()
      this.#lifetime.abort()
    }
    await Promise.allSettled([...this.#tasks])
  }

  async #renew(signal: AbortSignal): Promise<void> {
    while (!signal.aborted && !this.#closed) {
      await this.#send(V2_PEER_PATH_CONTROL_KIND.demand, signal).catch(() => undefined)
      if (signal.aborted || this.#closed) return
      await this.#clock.sleep(DEMAND_RENEWAL_MILLISECONDS, signal)
    }
  }

  #track(task: Promise<void>): void {
    const settled = task.catch(() => undefined).finally(() => this.#tasks.delete(settled))
    this.#tasks.add(settled)
  }

  #send(kind: number, signal = this.#lifetime.signal): Promise<void> {
    const revoke = kind === V2_PEER_PATH_CONTROL_KIND.revoke
    // The session retains its serialized transport write; demand cancellation
    // releases this owner's wait so shutdown cannot depend on a stalled relay.
    return awaitWithAbort(this.#session.sendPeerPathControl(encodeV2PeerPathControl({
      peerPathId: this.#path, networkGenerationId: this.#network.copyBytes(),
      controlSequence: ++this.#sequence, kind,
      validForMilliseconds: revoke ? 0 : DEMAND_LIFETIME_MILLISECONDS,
      holdForMilliseconds: revoke ? 0 : ATTEMPT_HOLD_MILLISECONDS,
      providerProfile: this.#providerProfile,
    }), { signal }), signal)
  }
}
