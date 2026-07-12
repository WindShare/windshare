import { describe, expect, it } from 'vitest'

import {
  BrowserReceiverConnectivity,
  type ConnectivitySignal,
  type OfferChannelFactory,
  type PeerChannel,
  type RelayConnectivityChannel,
  type SignalingRoute,
} from '../../src/connectivity'
import { createChunkIndex } from '../../src/contracts/selection'
import { ReceiveSession, encodeBlock } from '../../src/session'
import {
  MockFrameChannel,
  TestSink,
  sentRequest,
  settle,
  transferPlan,
} from '../session/helpers'

const identityOpener = {
  open: (_index: ReturnType<typeof createChunkIndex>, ciphertext: Uint8Array) =>
    ciphertext.slice(),
}

describe('D4 scheduler integration', () => {
  it('admits late P2P into the same session and aggregates without duplicate writes', async () => {
    const sink = new CountingSink()
    const session = new ReceiveSession(transferPlan(0, 12), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    const offers = new DeferredOfferFactory()
    const relay = new PolicyRelayChannel()
    const policy = new BrowserReceiverConnectivity(session, offers, {
      createSignalingRoute: () => relay.route,
    })
    await policy.start(1, relay, new AbortController().signal)
    const completed = session.start()
    await settle()
    expect(sentRequest(relay)).toEqual([0, 1, 2, 3, 4, 5, 6, 7])

    const peer = new PolicyPeerChannel()
    offers.resolve(peer)
    await settle()
    expect(sentRequest(peer)).toEqual([8, 9, 10, 11])

    for (let index = 8; index < 12; index += 1) {
      peer.push(response(index))
    }
    for (let index = 0; index < 8; index += 1) {
      relay.push(response(index))
    }
    // A late frame on the wrong path cannot race a second sink write.
    peer.push(response(0, 0xff))
    await completed

    expect(sink.writes.map(({ index }) => index).sort((a, b) => a - b)).toEqual(
      Array.from({ length: 12 }, (_, index) => index),
    )
    expect(sink.finalizeCalls).toBe(1)
    expect(session.state).toBe('completed')
    await policy.close()
  })
})

function response(index: number, payload = index): Uint8Array {
  return encodeBlock({
    index: BigInt(index),
    sequence: 0,
    last: true,
    payload: Uint8Array.of(payload),
  })
}

class CountingSink extends TestSink {
  finalizeCalls = 0

  override async finalize(): Promise<void> {
    this.finalizeCalls += 1
    await super.finalize()
  }
}

class DeferredOfferFactory implements OfferChannelFactory {
  #resolve!: (channel: PeerChannel) => void
  readonly #opening = new Promise<PeerChannel>((resolve) => {
    this.#resolve = resolve
  })

  offer(): Promise<PeerChannel> {
    return this.#opening
  }

  resolve(channel: PeerChannel): void {
    this.#resolve(channel)
  }
}

class PolicyPeerChannel extends MockFrameChannel implements PeerChannel {
  readonly opened = Promise.resolve()
  readonly done: Promise<void>
  reason: unknown
  #finish!: () => void

  constructor() {
    super()
    this.done = new Promise((resolve) => {
      this.#finish = resolve
    })
  }

  override async close(): Promise<void> {
    await super.close()
    this.#finish()
  }
}

class PolicyRelayChannel extends MockFrameChannel implements RelayConnectivityChannel {
  readonly route = new EmptyRoute()
  readonly signalMessages = this.route.messages

  sendSignal(kind: string, payload: unknown, signal?: AbortSignal): Promise<void> {
    return this.route.send({ kind, payload }, signal)
  }
}

class EmptyRoute implements SignalingRoute {
  readonly messages = new ReadableStream<ConnectivitySignal>()

  send(_signal: ConnectivitySignal, abort?: AbortSignal): Promise<void> {
    abort?.throwIfAborted()
    return Promise.resolve()
  }
}
