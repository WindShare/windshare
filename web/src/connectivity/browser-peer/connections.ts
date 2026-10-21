import type { PeerProviderFact } from '../peer-set/provider-facts'
import { browserReceiveBufferPolicy, type BrowserIdentity, type ReceiveBufferPolicy } from './receive-buffer-policy'

type ReceiveBufferFact = Extract<PeerProviderFact, { kind: 'receive-buffer' }>
type Observe = (fact: ReceiveBufferFact) => void
type DescriptionOperation = 'create-offer' | 'set-local-description' | 'set-remote-description'
const MAXIMUM_BROWSER_ERROR_NAME_CHARACTERS = 128

export interface BrowserPeerConnectionsOptions {
  readonly createPeerConnection?: (configuration: RTCConfiguration) => RTCPeerConnection
  readonly browser?: BrowserIdentity
}

/** Session-owned: direct and TURN attempts share a bounded compatibility downgrade. */
export class BrowserPeerConnections {
  readonly #create: (configuration: RTCConfiguration) => RTCPeerConnection
  #policy: ReceiveBufferPolicy

  constructor(options: BrowserPeerConnectionsOptions = {}) {
    this.#create = options.createPeerConnection ?? ((configuration) => new RTCPeerConnection(configuration))
    this.#policy = browserReceiveBufferPolicy(options.browser ?? globalThis.navigator)
  }

  create(configuration: RTCConfiguration, observe: Observe): BrowserPeerConnection {
    const policy = this.#policy
    const report: Observe = (fact) => {
      try { observe(fact) } catch { /* Diagnostics cannot acquire connection authority. */ }
    }
    const disable = (reason: string, cause?: unknown) => {
      this.#policy = { strategy: 'data-only', reason }
      report({ kind: 'receive-buffer', strategy: 'chromium-video', stage: 'disabled', reason,
        errorName: cause instanceof Error ? cause.name.slice(0, MAXIMUM_BROWSER_ERROR_NAME_CHARACTERS) : 'none' })
    }
    // WindShare peers support BUNDLE. Gathering per media type would multiply
    // ICE sockets and TURN allocations merely to obtain a shared socket buffer.
    const create = () => this.#create({ ...structuredClone(configuration), bundlePolicy: 'max-bundle' })
    const peer = create()
    report({ kind: 'receive-buffer', strategy: policy.strategy, stage: 'selected',
      reason: policy.reason, errorName: 'none' })
    if (policy.strategy === 'data-only') return new BrowserPeerConnection(peer, undefined, report, disable)
    if (typeof peer.addTransceiver !== 'function') {
      disable('transceiver-api-unavailable')
      return new BrowserPeerConnection(peer, undefined, report, disable)
    }
    let transceiver: RTCRtpTransceiver
    try {
      // Chromium applies its larger RTP receive buffer to the bundled UDP socket.
      // No capture source or media frames are involved. This is not a WebRTC
      // guarantee: https://webrtc.googlesource.com/src/+/2514dd7a20f187ce230e0d00a24b5fd33d29e0f2
      transceiver = peer.addTransceiver('video', { direction: 'recvonly' })
    } catch (cause) {
      closeBrowserPeer(peer)
      disable('add-transceiver-failed', cause)
      // Nothing has been signaled. Discard potentially partial browser state
      // before preparing the ordinary connection in the same attempt budget.
      return new BrowserPeerConnection(create(), undefined, report, disable)
    }
    report({ kind: 'receive-buffer', strategy: policy.strategy, stage: 'applied',
      reason: 'auxiliary-video-created', errorName: 'none' })
    return new BrowserPeerConnection(peer, transceiver, report, disable)
  }
}

export class BrowserPeerConnection {
  readonly peer: RTCPeerConnection
  readonly #receiveBufferTransceiver: RTCRtpTransceiver | undefined
  readonly #observe: Observe
  readonly #disable: (reason: string, cause?: unknown) => void

  constructor(
    peer: RTCPeerConnection,
    receiveBufferTransceiver: RTCRtpTransceiver | undefined,
    observe: Observe,
    disable: (reason: string, cause?: unknown) => void,
  ) {
    this.peer = peer
    this.#receiveBufferTransceiver = receiveBufferTransceiver
    this.#observe = observe
    this.#disable = disable
  }

  descriptionFailed(operation: DescriptionOperation, cause: unknown): void {
    if (this.#receiveBufferTransceiver === undefined) return
    // Native description failures can involve media capabilities. Network loss,
    // cancellation, invalid signaling and admission failures never call this.
    // Recovery owns the next authenticated attempt; do not replay an offer here.
    this.#disable(`${operation}-failed`, cause)
  }

  answerApplied(): void {
    if (this.#receiveBufferTransceiver === undefined) return
    if (this.#receiveBufferTransceiver.currentDirection !== 'recvonly') {
      this.#disable('auxiliary-video-not-negotiated')
      return
    }
    this.#observe({ kind: 'receive-buffer', strategy: 'chromium-video', stage: 'negotiated',
      reason: 'auxiliary-video-receive-only', errorName: 'none' })
    // Retain the transceiver until peer.close(): stopping it can renegotiate the
    // shared transport. Negotiated media does not prove the kernel buffer size.
  }
}

export function closeBrowserPeer(peer: RTCPeerConnection): void {
  try { peer.close() } catch {
    // Preserve the primary failure if the browser also throws during teardown.
  }
}
