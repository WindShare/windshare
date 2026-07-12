import { describe, expect, it, vi } from 'vitest'

import type { CapabilityLink, ValidatedManifestV1 } from '../../src/contracts'
import type {
  ConnectivityClock,
  ConnectivitySignal,
  OfferChannelFactory,
  PeerChannel,
  SignalingRoute,
} from '../../src/connectivity'
import {
  BrowserReceiverGateway,
  RELAY_REJOIN_RETRY_DELAY_MS,
  type BrowserGatewayRuntime,
} from '../../src/ui/browser-gateway'
import type { ReceiverTransferObserver } from '../../src/ui/model'
import { MockFrameChannel, settle } from '../session/helpers'

const FILE_MANIFEST = Object.freeze({
  version: 1,
  chunkSize: 1024,
  entries: Object.freeze([
    Object.freeze({ kind: 'file', path: 'file.bin', size: 1024, mtime: 0 }),
  ]),
}) as unknown as ValidatedManifestV1

describe('gateway relay recovery timing', () => {
  it('uses the injected clock between retries and admits the replacement', async () => {
    const clock = new ManualClock()
    const replacement = new FakeConnection()
    const initial = new FakeConnection(async (attempt) => {
      if (attempt === 1) {
        throw new Error('synthetic first rejoin failure')
      }
      return replacement
    })
    const offerFactory = new AbortOnlyOfferFactory()
    const runtime = {
      dialReceiver: async () => initial,
      openManifest: async () => ({
        manifest: FILE_MANIFEST,
        fingerprint: new Uint8Array(32),
      }),
      offerFactory,
      connectivityClock: clock,
    } as unknown as BrowserGatewayRuntime
    const outputAbort = vi.fn(async () => undefined)
    const gateway = new BrowserReceiverGateway(
      async () => ({
        transferTarget: (receiver) => receiver(
          {
            kind: 'single-file-stream' as const,
            output: new WritableStream<Uint8Array>(),
          },
        ),
        commit: async () => undefined,
        abort: outputAbort,
      }),
      [],
      runtime,
    )
    const controller = new AbortController()
    const share = await gateway.join(capability(), controller.signal)
    const plan = await gateway.compileSelection(share, null, controller.signal)
    const started = deferred<void>()
    const reconnected = vi.fn()
    const observer: ReceiverTransferObserver = {
      started: () => started.resolve(undefined),
      progress: vi.fn(),
      reconnecting: vi.fn(),
      reconnected,
    }
    const transfer = gateway.start(share, plan, 'download', observer, controller.signal)
    await started.promise

    initial.disconnect()
    await settle(24)
    expect(initial.rejoinCalls).toBe(1)
    expect(clock.pendingDurations).toEqual([RELAY_REJOIN_RETRY_DELAY_MS])

    clock.advance(RELAY_REJOIN_RETRY_DELAY_MS - 1)
    await settle()
    expect(initial.rejoinCalls).toBe(1)
    clock.advance(1)
    await settle(24)
    expect(initial.rejoinCalls).toBe(2)
    expect(reconnected).toHaveBeenCalledOnce()

    controller.abort(new DOMException('test complete', 'AbortError'))
    await expect(transfer).rejects.toMatchObject({ name: 'AbortError' })
    expect(outputAbort).toHaveBeenCalledOnce()
  })
})

function capability(): CapabilityLink {
  return {
    suite: 1,
    shareId: 'AAECAwQFBgcI',
    readSecret: new Uint8Array(16),
    relayHints: ['https://relay.test'],
  } as unknown as CapabilityLink
}

class FakeConnection {
  readonly channel = new RelayTestChannel()
  readonly sealedManifest = new Uint8Array()
  readonly done: Promise<void>
  rejoinCalls = 0
  readonly #rejoin: ((attempt: number) => Promise<FakeConnection>) | undefined
  #finish!: () => void

  constructor(rejoin?: (attempt: number) => Promise<FakeConnection>) {
    this.#rejoin = rejoin
    this.done = new Promise((resolve) => {
      this.#finish = resolve
    })
  }

  rejoin(): Promise<FakeConnection> {
    this.rejoinCalls += 1
    return this.#rejoin?.(this.rejoinCalls) ?? Promise.reject(new Error('unexpected rejoin'))
  }

  disconnect(): void {
    this.channel.remoteClose()
    this.#finish()
  }

  async close(): Promise<void> {
    await this.channel.close()
    this.#finish()
  }
}

class RelayTestChannel extends MockFrameChannel {
  readonly signalMessages = new ReadableStream<ConnectivitySignal>()

  sendSignal(_kind: string, _payload: unknown, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    return Promise.resolve()
  }
}

class AbortOnlyOfferFactory implements OfferChannelFactory {
  offer(_route: SignalingRoute, signal: AbortSignal): Promise<PeerChannel> {
    return new Promise((_resolve, reject) => {
      const aborted = () => reject(signal.reason)
      signal.addEventListener('abort', aborted, { once: true })
    })
  }
}

class ManualClock implements ConnectivityClock {
  #now = 0
  readonly #timers = new Set<ManualTimer>()

  get pendingDurations(): number[] {
    return [...this.#timers].map((timer) => timer.deadline - this.#now)
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

  #finish(settleTimer: () => void): void {
    if (this.#done) {
      return
    }
    this.#done = true
    this.#signal?.removeEventListener('abort', this.#abort)
    this.#settled()
    settleTimer()
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accept) => {
    resolve = accept
  })
  return { promise, resolve }
}
