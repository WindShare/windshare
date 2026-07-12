import { describe, expect, it, vi } from 'vitest'

import type { CapabilityLink, ValidatedManifestV1 } from '../../src/contracts'
import type {
  ConnectivitySignal,
  OfferChannelFactory,
  PeerChannel,
  SignalingRoute,
} from '../../src/connectivity'
import {
  BrowserReceiverGateway,
  type BrowserGatewayRuntime,
} from '../../src/ui/browser-gateway'
import type { ReceiverTransferObserver } from '../../src/ui/model'

const EMPTY_MANIFEST = Object.freeze({
  version: 1,
  chunkSize: 1024,
  entries: Object.freeze([]),
}) as unknown as ValidatedManifestV1

describe('browser gateway D4 composition', () => {
  it('keeps offer/ICE behind start and preserves picker-first ordering', async () => {
    const order: string[] = []
    const relay = new FakeRelayConnection()
    const offers = new GestureOfferFactory(order)
    const runtime = {
      dialReceiver: async () => relay,
      openManifest: async () => ({
        manifest: EMPTY_MANIFEST,
        fingerprint: new Uint8Array(32),
      }),
      offerFactory: offers,
    } as unknown as BrowserGatewayRuntime
    const commit = vi.fn(async () => undefined)
    const gateway = new BrowserReceiverGateway(
      () => {
        order.push('picker')
        return Promise.resolve({
          transferTarget: (receiver) => receiver(
            {
              kind: 'zip-stream' as const,
              output: new WritableStream<Uint8Array>(),
            },
          ),
          commit,
          abort: async () => undefined,
        })
      },
      [],
      runtime,
    )
    const controller = new AbortController()
    const share = await gateway.join(capability(), controller.signal)
    const plan = await gateway.compileSelection(share, null, controller.signal)

    expect(offers.calls).toBe(0)
    expect(order).toEqual([])

    await gateway.start(share, plan, 'download', observer(), controller.signal)
    expect(order.slice(0, 2)).toEqual(['picker', 'offer'])
    expect(offers.calls).toBe(1)
    expect(commit).toHaveBeenCalledOnce()
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

function observer(): ReceiverTransferObserver {
  return {
    started: vi.fn(),
    progress: vi.fn(),
    reconnecting: vi.fn(),
    reconnected: vi.fn(),
  }
}

class GestureOfferFactory implements OfferChannelFactory {
  calls = 0
  readonly #order: string[]

  constructor(order: string[]) {
    this.#order = order
  }

  offer(_route: SignalingRoute, signal: AbortSignal): Promise<PeerChannel> {
    this.calls += 1
    this.#order.push('offer')
    return new Promise((_resolve, reject) => {
      const aborted = () => reject(signal.reason)
      signal.addEventListener('abort', aborted, { once: true })
    })
  }
}

class FakeRelayConnection {
  readonly channel = new FakeRelayChannel()
  readonly sealedManifest = new Uint8Array()
  readonly done: Promise<void>
  #finish!: () => void

  constructor() {
    this.done = new Promise((resolve) => {
      this.#finish = resolve
    })
  }

  rejoin(): Promise<FakeRelayConnection> {
    return Promise.reject(new Error('rejoin is not expected'))
  }

  async close(): Promise<void> {
    await this.channel.close()
    this.#finish()
  }
}

class FakeRelayChannel {
  readonly frames: ReadableStream<Uint8Array>
  readonly signalMessages = new ReadableStream<ConnectivitySignal>()
  state: 'open' | 'closed' = 'open'
  #closeFrames!: () => void

  constructor() {
    this.frames = new ReadableStream({
      start: (controller) => {
        this.#closeFrames = () => controller.close()
      },
    })
  }

  send(): Promise<void> {
    return Promise.resolve()
  }

  sendSignal(): Promise<void> {
    return Promise.resolve()
  }

  async sendTerminal(): Promise<void> {
    await this.close()
  }

  close(): Promise<void> {
    if (this.state === 'open') {
      this.state = 'closed'
      this.#closeFrames()
    }
    return Promise.resolve()
  }
}
