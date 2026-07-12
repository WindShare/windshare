import { describe, expect, it, vi } from 'vitest'

import type { FrameChannel } from '../../src/contracts'
import {
  BrowserReceiverConnectivity,
  P2P_CONNECT_TIMEOUT_MS,
  SMALL_SHARE_BYTES,
  type ConnectivityClock,
  type ConnectivitySignal,
  type OfferChannelFactory,
  type PeerChannel,
  type RelayConnectivityChannel,
  type SignalingRoute,
} from '../../src/connectivity'

describe('browser receiver connection policy', () => {
  it('does nothing before start and admits relay first at the small-share boundary', async () => {
    const fixture = policyFixture()
    expect(fixture.offers.attempts).toHaveLength(0)
    expect(fixture.pool.channels).toHaveLength(0)

    await fixture.policy.start(SMALL_SHARE_BYTES, fixture.relay, fixture.signal)
    expect(fixture.pool.channels).toEqual([fixture.relay])
    expect(fixture.offers.attempts).toHaveLength(1)

    const peer = new FakePeerChannel()
    fixture.offers.attempts[0]?.resolve(peer)
    await settle()
    expect(fixture.pool.channels).toEqual([fixture.relay, peer])
    await fixture.policy.close()
  })

  it('waits exactly ten seconds for a large share, then aggregates a late peer', async () => {
    const fixture = policyFixture()
    let started = false
    const starting = fixture.policy
      .start(SMALL_SHARE_BYTES + 1, fixture.relay, fixture.signal)
      .then(() => {
        started = true
      })
    await settle()

    expect(fixture.pool.channels).toHaveLength(0)
    fixture.clock.advance(P2P_CONNECT_TIMEOUT_MS - 1)
    await settle()
    expect(started).toBe(false)

    fixture.clock.advance(1)
    await starting
    expect(fixture.pool.channels).toEqual([fixture.relay])

    const peer = new FakePeerChannel()
    fixture.offers.attempts[0]?.resolve(peer)
    await settle()
    expect(fixture.pool.channels).toEqual([fixture.relay, peer])
    await fixture.policy.close()
  })

  it('starts a large share P2P-only when Open wins and cancels the fallback timer', async () => {
    const fixture = policyFixture()
    const starting = fixture.policy.start(
      SMALL_SHARE_BYTES + 1,
      fixture.relay,
      fixture.signal,
    )
    const peer = new FakePeerChannel()
    fixture.offers.attempts[0]?.resolve(peer)

    await starting
    expect(fixture.pool.channels).toEqual([peer])
    expect(fixture.clock.pending).toBe(0)

    peer.finish(new Error('peer path lost'))
    await settle()
    expect(fixture.pool.channels).toEqual([peer, fixture.relay])
    await fixture.policy.close()
  })

  it('falls back immediately when P2P negotiation fails before the timeout', async () => {
    const onPeerError = vi.fn()
    const fixture = policyFixture({ onPeerError })
    const starting = fixture.policy.start(
      SMALL_SHARE_BYTES + 1,
      fixture.relay,
      fixture.signal,
    )
    const failure = new Error('synthetic negotiation failure')
    fixture.offers.attempts[0]?.reject(failure)

    await starting
    expect(fixture.pool.channels).toEqual([fixture.relay])
    expect(onPeerError).toHaveBeenCalledWith(failure)
    expect(fixture.clock.pending).toBe(0)
    await fixture.policy.close()
  })

  it('closes a late peer after cancellation without admitting it', async () => {
    const fixture = policyFixture({ ignoreOfferAbort: true })
    const starting = fixture.policy.start(
      SMALL_SHARE_BYTES + 1,
      fixture.relay,
      fixture.signal,
    )
    fixture.controller.abort(new DOMException('cancel transfer', 'AbortError'))
    await expect(starting).rejects.toMatchObject({ name: 'AbortError' })

    const late = new FakePeerChannel()
    fixture.offers.attempts[0]?.resolve(late)
    await settle()
    expect(fixture.pool.channels).toHaveLength(0)
    expect(late.closeCalls).toBe(1)
    await fixture.policy.close()
  })

  it('retains the established peer owner until cancellation tears down the transfer', async () => {
    const fixture = policyFixture()
    await fixture.policy.start(1, fixture.relay, fixture.signal)
    const attempt = fixture.offers.attempts[0]!
    const peer = new FakePeerChannel()
    attempt.resolve(peer)
    await settle()

    expect(attempt.signal.aborted).toBe(false)
    fixture.controller.abort(new DOMException('cancel established peer', 'AbortError'))
    expect(attempt.signal.aborted).toBe(true)
    await fixture.policy.close()
  })

  it('retries only on the newest relay generation and closes stale success', async () => {
    const fixture = policyFixture({ ignoreOfferAbort: true })
    await fixture.policy.start(1, fixture.relay, fixture.signal)
    const replacement = new FakeRelayChannel()
    fixture.policy.replaceRelay(replacement)
    expect(fixture.offers.attempts).toHaveLength(2)

    const stale = new FakePeerChannel()
    fixture.offers.attempts[0]?.resolve(stale)
    await settle()
    expect(stale.closeCalls).toBe(1)
    expect(fixture.pool.channels).toEqual([fixture.relay, replacement])

    const current = new FakePeerChannel()
    fixture.offers.attempts[1]?.resolve(current)
    await settle()
    expect(fixture.pool.channels).toEqual([fixture.relay, replacement, current])
    await fixture.policy.close()
  })

  it('keeps a healthy peer across relay rejoin and retries after that peer ends', async () => {
    const fixture = policyFixture()
    await fixture.policy.start(1, fixture.relay, fixture.signal)
    const peer = new FakePeerChannel()
    fixture.offers.attempts[0]?.resolve(peer)
    await settle()

    const replacement = new FakeRelayChannel()
    fixture.policy.replaceRelay(replacement)
    expect(fixture.offers.attempts).toHaveLength(1)
    expect(fixture.policy.peerAvailable).toBe(true)

    peer.finish()
    await settle()
    expect(fixture.offers.attempts).toHaveLength(2)
    expect(fixture.offers.attempts[1]?.route).toBe(replacement.route)
    await fixture.policy.close()
  })

  it('does not retry a newer signaling generation after the session starts finalizing', async () => {
    const fixture = policyFixture()
    await fixture.policy.start(1, fixture.relay, fixture.signal)
    const peer = new FakePeerChannel()
    fixture.offers.attempts[0]?.resolve(peer)
    await settle()
    fixture.policy.replaceRelay(new FakeRelayChannel())

    fixture.pool.state = 'finalizing'
    peer.finish()
    await settle()
    expect(fixture.offers.attempts).toHaveLength(1)
    await fixture.policy.close()
  })
})

function policyFixture(options: {
  readonly ignoreOfferAbort?: boolean
  readonly onPeerError?: (error: unknown) => void
} = {}) {
  const pool = new FakePool()
  const offers = new ScriptedOfferFactory(options.ignoreOfferAbort ?? false)
  const clock = new ManualClock()
  const relay = new FakeRelayChannel()
  const controller = new AbortController()
  const policy = new BrowserReceiverConnectivity(pool, offers, {
    clock,
    ...(options.onPeerError === undefined ? {} : { onPeerError: options.onPeerError }),
    createSignalingRoute: (channel) => (channel as FakeRelayChannel).route,
  })
  return {
    pool,
    offers,
    clock,
    relay,
    controller,
    policy,
    signal: controller.signal,
  }
}

class FakePool {
  readonly channels: FrameChannel[] = []
  state: 'finalizing' | 'idle' | 'running' = 'idle'

  addChannel(channel: FrameChannel): void {
    this.channels.push(channel)
  }
}

class ScriptedOfferFactory implements OfferChannelFactory {
  readonly attempts: OfferAttempt[] = []
  readonly #ignoreAbort: boolean

  constructor(ignoreAbort: boolean) {
    this.#ignoreAbort = ignoreAbort
  }

  offer(route: SignalingRoute, signal: AbortSignal): Promise<PeerChannel> {
    const attempt = new OfferAttempt(route, signal)
    this.attempts.push(attempt)
    if (!this.#ignoreAbort) {
      const aborted = () => attempt.reject(signal.reason)
      signal.addEventListener('abort', aborted, { once: true })
      attempt.promise.then(
        () => signal.removeEventListener('abort', aborted),
        () => signal.removeEventListener('abort', aborted),
      )
    }
    return attempt.promise
  }
}

class OfferAttempt {
  readonly route: SignalingRoute
  readonly signal: AbortSignal
  readonly promise: Promise<PeerChannel>
  #resolve!: (channel: PeerChannel) => void
  #reject!: (error: unknown) => void

  constructor(route: SignalingRoute, signal: AbortSignal) {
    this.route = route
    this.signal = signal
    this.promise = new Promise((resolve, reject) => {
      this.#resolve = resolve
      this.#reject = reject
    })
  }

  resolve(channel: PeerChannel): void {
    this.#resolve(channel)
  }

  reject(error: unknown): void {
    this.#reject(error)
  }
}

class FakePeerChannel implements PeerChannel {
  readonly opened = Promise.resolve()
  readonly frames: ReadableStream<Uint8Array>
  readonly done: Promise<void>
  state: 'open' | 'closed' = 'open'
  reason: unknown
  closeCalls = 0
  #closeFrames!: () => void
  #finish!: () => void

  constructor() {
    this.frames = new ReadableStream({
      start: (controller) => {
        this.#closeFrames = () => controller.close()
      },
    })
    this.done = new Promise((resolve) => {
      this.#finish = resolve
    })
  }

  send(): Promise<void> {
    return Promise.resolve()
  }

  sendTerminal(): Promise<void> {
    this.finish()
    return Promise.resolve()
  }

  close(): Promise<void> {
    this.closeCalls += 1
    this.finish()
    return Promise.resolve()
  }

  finish(reason?: unknown): void {
    if (this.state === 'closed') {
      return
    }
    this.reason = reason
    this.state = 'closed'
    this.#closeFrames()
    this.#finish()
  }
}

class FakeRelayChannel extends FakePeerChannel implements RelayConnectivityChannel {
  readonly route = new FakeRoute()
  readonly signalMessages = this.route.messages

  sendSignal(kind: string, payload: unknown, signal?: AbortSignal): Promise<void> {
    return this.route.send({ kind, payload }, signal)
  }
}

class FakeRoute implements SignalingRoute {
  readonly messages = new ReadableStream<ConnectivitySignal>()
  readonly sent: ConnectivitySignal[] = []

  send(signal: ConnectivitySignal, abort?: AbortSignal): Promise<void> {
    abort?.throwIfAborted()
    this.sent.push(structuredClone(signal))
    return Promise.resolve()
  }
}

class ManualClock implements ConnectivityClock {
  #now = 0
  readonly #timers = new Set<ManualTimer>()

  get pending(): number {
    return this.#timers.size
  }

  sleep(milliseconds: number, signal?: AbortSignal): Promise<void> {
    const timer = new ManualTimer(this.#now + milliseconds, signal, () => {
      this.#timers.delete(timer)
    })
    this.#timers.add(timer)
    return timer.promise
  }

  advance(milliseconds: number): void {
    this.#now += milliseconds
    for (const timer of [...this.#timers]) {
      if (timer.deadline <= this.#now) {
        timer.resolve()
      }
    }
  }
}

class ManualTimer {
  readonly deadline: number
  readonly promise: Promise<void>
  readonly #signal: AbortSignal | undefined
  readonly #settled: () => void
  #resolve!: () => void
  #reject!: (reason: unknown) => void
  #done = false

  constructor(deadline: number, signal: AbortSignal | undefined, settled: () => void) {
    this.deadline = deadline
    this.#signal = signal
    this.#settled = settled
    this.promise = new Promise((resolve, reject) => {
      this.#resolve = resolve
      this.#reject = reject
    })
    signal?.addEventListener('abort', this.#abort, { once: true })
  }

  resolve(): void {
    this.#finish(() => this.#resolve())
  }

  #abort = (): void => {
    this.#finish(() => this.#reject(this.#signal?.reason))
  }

  #finish(settle: () => void): void {
    if (this.#done) {
      return
    }
    this.#done = true
    this.#signal?.removeEventListener('abort', this.#abort)
    this.#settled()
    settle()
  }
}

async function settle(turns = 12): Promise<void> {
  for (let turn = 0; turn < turns; turn += 1) {
    await Promise.resolve()
  }
}
