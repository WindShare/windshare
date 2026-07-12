import type { FrameChannel } from '../contracts'
import { abortReason, browserConnectivityClock, type ConnectivityClock } from './clock'
import { ReceiverConnectivityClosedError } from './errors'
import type { PeerChannel } from './peer-channel'
import type { OfferChannelFactory } from './peer-offer'
import {
  RelaySignalingRoute,
  type RelayConnectivityChannel,
  type SignalingRoute,
} from './signaling'

export const SMALL_SHARE_BYTES = 8 * 1024 * 1024
export const P2P_CONNECT_TIMEOUT_MS = 10_000

export interface ReceiverChannelPool {
  readonly state?: 'closed' | 'completed' | 'failed' | 'finalizing' | 'idle' | 'running'
  addChannel(channel: FrameChannel): void
}

export interface ReceiverConnectivityOptions {
  readonly clock?: ConnectivityClock
  readonly onPeerError?: (error: unknown) => void
  readonly createSignalingRoute?: (channel: RelayConnectivityChannel) => SignalingRoute
}

interface RelayGeneration {
  readonly number: number
  readonly channel: RelayConnectivityChannel
  readonly route: SignalingRoute
}

interface PeerAttempt {
  readonly generation: number
  readonly controller: AbortController
  readonly outcome: Promise<'failed' | 'opened'>
  readonly opened: () => void
  readonly failed: () => void
}

/**
 * Owns browser path admission while leaving frame scheduling in ReceiveSession.
 * One long-lived pool therefore aggregates relay and P2P without duplicate sink
 * or completion ownership.
 */
export class BrowserReceiverConnectivity {
  readonly #pool: ReceiverChannelPool
  readonly #offers: OfferChannelFactory
  readonly #clock: ConnectivityClock
  readonly #onPeerError: (error: unknown) => void
  readonly #createSignalingRoute: (channel: RelayConnectivityChannel) => SignalingRoute
  readonly #lifetime = new AbortController()
  readonly #admitted = new WeakSet<FrameChannel>()
  readonly #tasks = new Set<Promise<void>>()
  #externalSignal: AbortSignal | undefined
  #currentRelay: RelayGeneration | undefined
  #attempt: PeerAttempt | undefined
  #peer: PeerChannel | undefined
  #peerController: AbortController | undefined
  #started = false
  #closed = false

  constructor(
    pool: ReceiverChannelPool,
    offers: OfferChannelFactory,
    options: ReceiverConnectivityOptions = {},
  ) {
    this.#pool = pool
    this.#offers = offers
    this.#clock = options.clock ?? browserConnectivityClock
    this.#onPeerError = options.onPeerError ?? (() => undefined)
    this.#createSignalingRoute = options.createSignalingRoute ??
      ((channel) => new RelaySignalingRoute(channel))
  }

  get peerAvailable(): boolean {
    return this.#peer?.state === 'open'
  }

  async start(
    selectedBytes: number,
    relay: RelayConnectivityChannel,
    signal: AbortSignal,
  ): Promise<void> {
    if (this.#started) {
      throw new Error('browser receiver connectivity can only be started once')
    }
    if (!Number.isSafeInteger(selectedBytes) || selectedBytes < 0) {
      throw new RangeError('selected byte count must be a non-negative safe integer')
    }
    this.#started = true
    this.#externalSignal = signal
    signal.addEventListener('abort', this.#abortFromCaller, { once: true })
    if (signal.aborted) {
      this.#abortFromCaller()
    }
    this.#requireActive()
    const current = this.#installRelay(relay)

    if (selectedBytes <= SMALL_SHARE_BYTES) {
      this.#admit(current.channel)
      this.#beginAttempt(current)
      return
    }

    const attempt = this.#beginAttempt(current)
    const timeout = new AbortController()
    const abortTimeout = () => timeout.abort(abortReason(this.#lifetime.signal))
    this.#lifetime.signal.addEventListener('abort', abortTimeout, { once: true })
    try {
      const outcome = await Promise.race([
        attempt.outcome,
        this.#clock.sleep(P2P_CONNECT_TIMEOUT_MS, timeout.signal).then(() => 'timeout' as const),
      ])
      this.#requireActive()
      if (outcome !== 'opened') {
        this.#admit(current.channel)
      }
    } finally {
      this.#lifetime.signal.removeEventListener('abort', abortTimeout)
      timeout.abort(new DOMException('P2P wait settled', 'AbortError'))
    }
  }

  replaceRelay(relay: RelayConnectivityChannel): void {
    this.#requireActive()
    if (!this.#started) {
      throw new Error('browser receiver connectivity has not started')
    }
    if (!this.#poolAcceptsChannels()) {
      relay.close().catch(() => undefined)
      return
    }
    // A replacement is already on the transfer's critical path, so admit it
    // before changing signaling generations.
    this.#admit(relay)
    const current = this.#installRelay(relay)
    if (this.peerAvailable) {
      return
    }
    const staleAttempt = this.#attempt
    staleAttempt?.controller.abort(
      new DOMException('A newer relay signaling route is available', 'AbortError'),
    )
    if (this.#attempt === staleAttempt) {
      // The newest relay must not wait for an obsolete factory to acknowledge
      // cancellation. Any late success still fails #ownsAttempt and is closed.
      this.#attempt = undefined
    }
    this.#beginAttempt(current)
  }

  async close(): Promise<void> {
    if (this.#closed) {
      await Promise.allSettled([...this.#tasks])
      return
    }
    this.#closed = true
    this.#externalSignal?.removeEventListener('abort', this.#abortFromCaller)
    const reason = new ReceiverConnectivityClosedError()
    this.#lifetime.abort(reason)
    this.#attempt?.controller.abort(reason)
    // Once negotiation has published a channel, only its retained owner can
    // force parent-first teardown when DataChannel Close is terminal-blocked.
    this.#peerController?.abort(reason)
    await this.#peer?.close().catch(() => undefined)
    await Promise.allSettled([...this.#tasks])
  }

  #installRelay(channel: RelayConnectivityChannel): RelayGeneration {
    const current = {
      number: (this.#currentRelay?.number ?? 0) + 1,
      channel,
      route: this.#createSignalingRoute(channel),
    }
    this.#currentRelay = current
    return current
  }

  #beginAttempt(relay: RelayGeneration): PeerAttempt {
    if (this.#attempt !== undefined) {
      return this.#attempt
    }
    const controller = new AbortController()
    const settlement = attemptSettlement()
    const attempt: PeerAttempt = {
      generation: relay.number,
      controller,
      outcome: settlement.promise,
      opened: settlement.opened,
      failed: settlement.failed,
    }
    this.#attempt = attempt
    const task = this.#runAttempt(attempt, relay)
    this.#track(task)
    return attempt
  }

  async #runAttempt(attempt: PeerAttempt, relay: RelayGeneration): Promise<void> {
    const abortAttempt = () => attempt.controller.abort(abortReason(this.#lifetime.signal))
    this.#lifetime.signal.addEventListener('abort', abortAttempt, { once: true })
    let channel: PeerChannel | undefined
    try {
      channel = await this.#offers.offer(relay.route, attempt.controller.signal)
      if (!this.#ownsAttempt(attempt, relay) || channel.state !== 'open') {
        attempt.failed()
        await channel.close().catch(() => undefined)
        return
      }
      try {
        this.#pool.addChannel(channel)
      } catch (error) {
        await channel.close().catch(() => undefined)
        throw error
      }
      this.#peer = channel
      this.#peerController = attempt.controller
      attempt.opened()
      this.#track(this.#watchPeer(channel, attempt.controller, relay.number))
    } catch (error) {
      attempt.failed()
      if (this.#ownsAttempt(attempt, relay) && !attempt.controller.signal.aborted) {
        this.#onPeerError(error)
      }
    } finally {
      this.#lifetime.signal.removeEventListener('abort', abortAttempt)
      if (this.#attempt === attempt) {
        this.#attempt = undefined
      }
      const current = this.#currentRelay
      if (current !== undefined && current.number > attempt.generation &&
          !this.peerAvailable && !this.#closed && !this.#lifetime.signal.aborted &&
          this.#poolAcceptsChannels()) {
        this.#beginAttempt(current)
      }
    }
  }

  async #watchPeer(
    channel: PeerChannel,
    controller: AbortController,
    generation: number,
  ): Promise<void> {
    await channel.done
    if (this.#peer !== channel) {
      return
    }
    this.#peer = undefined
    if (this.#peerController === controller) {
      this.#peerController = undefined
    }
    if (this.#closed || this.#lifetime.signal.aborted || !this.#poolAcceptsChannels()) {
      return
    }
    const current = this.#currentRelay
    if (current === undefined) {
      return
    }
    // Large transfers may have started P2P-only. If that path disappears, the
    // still-open relay must enter the same scheduler before any retry decision.
    this.#admit(current.channel)
    if (current.number > generation && this.#attempt === undefined) {
      this.#beginAttempt(current)
    }
    if (channel.reason !== undefined) {
      this.#onPeerError(channel.reason)
    }
  }

  #ownsAttempt(attempt: PeerAttempt, relay: RelayGeneration): boolean {
    return !this.#closed && !this.#lifetime.signal.aborted &&
      this.#attempt === attempt && this.#currentRelay?.number === relay.number &&
      this.#poolAcceptsChannels()
  }

  #poolAcceptsChannels(): boolean {
    return this.#pool.state === undefined ||
      this.#pool.state === 'idle' || this.#pool.state === 'running'
  }

  #admit(channel: FrameChannel): void {
    if (this.#admitted.has(channel)) {
      return
    }
    this.#requireActive()
    try {
      this.#pool.addChannel(channel)
      this.#admitted.add(channel)
    } catch (error) {
      channel.close().catch(() => undefined)
      throw error
    }
  }

  #track(task: Promise<void>): void {
    this.#tasks.add(task)
    task.finally(() => this.#tasks.delete(task)).catch(() => undefined)
  }

  #requireActive(): void {
    if (this.#closed) {
      throw new ReceiverConnectivityClosedError()
    }
    if (this.#lifetime.signal.aborted) {
      throw abortReason(this.#lifetime.signal)
    }
  }

  #abortFromCaller = (): void => {
    if (this.#externalSignal !== undefined) {
      const reason = abortReason(this.#externalSignal)
      this.#lifetime.abort(reason)
      this.#attempt?.controller.abort(reason)
      this.#peerController?.abort(reason)
    }
  }
}

function attemptSettlement(): {
  readonly promise: Promise<'failed' | 'opened'>
  readonly opened: () => void
  readonly failed: () => void
} {
  let settle!: (value: 'failed' | 'opened') => void
  let settled = false
  const promise = new Promise<'failed' | 'opened'>((resolve) => {
    settle = resolve
  })
  const resolve = (value: 'failed' | 'opened') => {
    if (!settled) {
      settled = true
      settle(value)
    }
  }
  return {
    promise,
    opened: () => resolve('opened'),
    failed: () => resolve('failed'),
  }
}
