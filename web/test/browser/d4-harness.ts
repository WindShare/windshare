import {
  BrowserOfferChannelFactory,
  SIGNAL_KIND_CANDIDATE,
  SIGNAL_KIND_OFFER,
  type ConnectivitySignal,
  type SignalingRoute,
} from '../../src/connectivity'
import {
  wrapWindShareDataChannel,
} from '../../src/transport/webrtc'

export interface D4BrowserResult {
  readonly peerCreationsBeforeOffer: number
  readonly peerCreationsAfterOffer: number
  readonly localFrames: readonly number[]
  readonly remoteFrames: readonly number[]
  readonly survivedSignalingLoss: boolean
  readonly terminalAcknowledged: boolean
  readonly localState: string
  readonly remoteState: string
}

export async function runD4BrowserLoopback(): Promise<D4BrowserResult> {
  const remotePeer = new RTCPeerConnection({ iceServers: [] })
  const remoteRaw = deferred<RTCDataChannel>()
  remotePeer.addEventListener('datachannel', (event) => {
    remoteRaw.resolve((event as RTCDataChannelEvent).channel)
  }, { once: true })
  const route = new BrowserLoopbackRoute(remotePeer)
  let peerCreations = 0
  const factory = new BrowserOfferChannelFactory({
    configuration: { iceServers: [] },
    createPeerConnection: (configuration) => {
      peerCreations += 1
      return new RTCPeerConnection(configuration)
    },
  })
  const peerCreationsBeforeOffer = peerCreations
  const abort = new AbortController()
  const local = await factory.offer(route, abort.signal)
  const remote = wrapWindShareDataChannel(remotePeer, await remoteRaw.promise)
  await remote.opened

  const localCollection = collectMarkers(local.frames)
  const remoteCollection = collectMarkers(remote.frames)
  await local.send(Uint8Array.of(0x11))
  await remote.send(Uint8Array.of(0x22))
  await Promise.all([localCollection.first, remoteCollection.first])
  route.close()
  await Promise.resolve()
  const survivedSignalingLoss = local.state === 'open'
  await local.send(Uint8Array.of(0x33))
  await local.sendTerminal(Uint8Array.of(0x44))
  await Promise.all([local.done, remote.done])
  const [localFrames, remoteFrames] = await Promise.all([
    localCollection.all,
    remoteCollection.all,
  ])
  const result: D4BrowserResult = {
    peerCreationsBeforeOffer,
    peerCreationsAfterOffer: peerCreations,
    localFrames,
    remoteFrames,
    survivedSignalingLoss,
    terminalAcknowledged: local.reason === undefined,
    localState: local.state,
    remoteState: remote.state,
  }
  await Promise.all([local.close(), remote.close()])
  remotePeer.close()
  return result
}

class BrowserLoopbackRoute implements SignalingRoute {
  readonly messages: ReadableStream<ConnectivitySignal>
  readonly #remote: RTCPeerConnection
  #controller!: ReadableStreamDefaultController<ConnectivitySignal>
  #closed = false

  constructor(remote: RTCPeerConnection) {
    this.#remote = remote
    this.messages = new ReadableStream({
      start: (controller) => {
        this.#controller = controller
      },
    })
    remote.addEventListener('icecandidate', (event) => {
      const candidate = (event as RTCPeerConnectionIceEvent).candidate
      if (candidate !== null && !this.#closed) {
        this.#controller.enqueue({
          kind: SIGNAL_KIND_CANDIDATE,
          payload: candidate.toJSON(),
        })
      }
    })
  }

  async send(signal: ConnectivitySignal, abort?: AbortSignal): Promise<void> {
    abort?.throwIfAborted()
    if (signal.kind === SIGNAL_KIND_CANDIDATE) {
      await this.#remote.addIceCandidate(signal.payload as RTCIceCandidateInit)
      return
    }
    if (signal.kind !== SIGNAL_KIND_OFFER) {
      throw new Error(`unexpected browser loopback signal ${signal.kind}`)
    }
    await this.#remote.setRemoteDescription(signal.payload as RTCSessionDescriptionInit)
    await this.#remote.setLocalDescription(await this.#remote.createAnswer())
    const answer = this.#remote.localDescription
    if (answer === null) {
      throw new Error('browser loopback answer is unavailable')
    }
    this.#controller.enqueue({
      kind: 'answer',
      payload: { type: answer.type, sdp: answer.sdp },
    })
  }

  close(): void {
    if (!this.#closed) {
      this.#closed = true
      this.#controller.close()
    }
  }
}

function collectMarkers(stream: ReadableStream<Uint8Array>): {
  readonly first: Promise<void>
  readonly all: Promise<number[]>
} {
  const first = deferred<void>()
  const markers: number[] = []
  const reader = stream.getReader()
  const all = (async () => {
    while (true) {
      const result = await reader.read()
      if (result.done) {
        return markers
      }
      markers.push(result.value[0] ?? 0)
      first.resolve(undefined)
    }
  })()
  return { first: first.promise, all }
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
