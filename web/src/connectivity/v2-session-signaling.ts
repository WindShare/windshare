import { encodeBase64Url } from '../crypto/bytes'
import {
  decodeV2OperationErrorControl,
  V2MessageError,
  type V2SessionMessage,
  V2_MESSAGE_KIND,
} from '../session/v2-message'
import type { V2ReceiverSessionRuntime, V2SessionOperation } from '../session/v2-runtime'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import {
  decodeV2PeerAnswer,
  decodeV2PeerCandidate,
  encodeV2PeerCandidate,
  encodeV2PeerOffer,
  sameV2PeerBinding,
  type V2PeerBinding,
  V2SignalingCodecError,
} from './v2-signaling-codec'
import {
  SIGNAL_KIND_ANSWER,
  SIGNAL_KIND_CANDIDATE,
  SIGNAL_KIND_OFFER,
  type ConnectivitySignal,
  type SignalingRoute,
} from './signaling'

export class V2SessionSignalingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2SessionSignalingError'
  }
}

type V2SessionSignalingDecision = {
  readonly type: 'route-failed'
  readonly failureScope: 'attempt' | 'session'
  readonly reason: unknown
}

export type V2SessionSignalingTrace = V2SessionSignalingDecision & {
  readonly peerPathId: string
  readonly attemptId: string
}

export type V2SessionSignalingObserver = (event: V2SessionSignalingTrace) => void

class V2PeerProtocolError extends V2SessionSignalingError {
  constructor(message: string) {
    super(message)
    this.name = 'V2PeerProtocolError'
  }
}

/**
 * Projects one PeerConnection negotiation over a single authenticated operation.
 * Path and attempt identities live here so content/session scheduling never learns
 * provider or ICE policy, while every trickled candidate remains bound to the offer.
 */
export class V2SessionSignalingRoute implements SignalingRoute {
  readonly messages: ReadableStream<ConnectivitySignal>
  readonly binding: V2PeerBinding
  readonly #session: V2ReceiverSessionRuntime
  readonly #controller: ReadableStreamDefaultController<ConnectivitySignal>
  readonly #observe: V2SessionSignalingObserver
  #operation: V2SessionOperation | undefined
  #offerStarted = false
  #closed = false
  #answerSeen = false

  constructor(
    session: V2ReceiverSessionRuntime,
    binding: V2PeerBinding,
    observe: V2SessionSignalingObserver = () => undefined,
  ) {
    this.#session = session
    this.binding = snapshotBinding(binding)
    this.#observe = observe
    let controller!: ReadableStreamDefaultController<ConnectivitySignal>
    this.messages = new ReadableStream<ConnectivitySignal>({
      start: (candidate) => {
        controller = candidate
      },
      cancel: () => this.close(),
    })
    this.#controller = controller
  }

  async send(message: ConnectivitySignal, signal?: AbortSignal): Promise<void> {
    this.#requireOpen()
    signal?.throwIfAborted()
    if (message.kind === SIGNAL_KIND_OFFER) {
      await this.#startOffer(message.payload, signal)
      return
    }
    if (message.kind !== SIGNAL_KIND_CANDIDATE) {
      throw new V2SessionSignalingError(`Receiver cannot send signal kind ${message.kind}`)
    }
    const operation = this.#operation
    if (operation === undefined) {
      throw new V2SessionSignalingError('ICE candidate arrived before the peer offer operation')
    }
    const candidate = candidatePayload(message.payload)
    await this.#session.sendOperationMessage(
      operation,
      V2_MESSAGE_KIND.peerCandidate,
      encodeV2PeerCandidate({ ...this.binding, ...candidate }),
      signal === undefined ? {} : { signal },
    )
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    const operation = this.#operation
    this.#operation = undefined
    if (operation !== undefined) {
      await this.#session.cancelOperation(operation, 2).catch(() => undefined)
    }
    try {
      this.#controller.close()
    } catch {
      // A consumer-originated cancellation already owns stream settlement.
    }
  }

  async #startOffer(payload: unknown, signal?: AbortSignal): Promise<void> {
    if (this.#offerStarted) throw new V2SessionSignalingError('Peer offer was sent more than once')
    this.#offerStarted = true
    const sdp = descriptionPayload(payload, SIGNAL_KIND_OFFER)
    try {
      const operation = await this.#session.beginOperation(
        V2_MESSAGE_KIND.peerOffer,
        encodeV2PeerOffer({ ...this.binding, sdp }),
        signal === undefined ? {} : { signal },
      )
      if (this.#closed) {
        await this.#session.cancelOperation(operation, 2).catch(() => undefined)
        return
      }
      this.#operation = operation
      this.#pump(operation).catch(() => undefined)
    } catch (error) {
      this.#fail(error)
      throw error
    }
  }

  async #pump(operation: V2SessionOperation): Promise<void> {
    try {
      while (!this.#closed && this.#operation === operation) {
        await this.#acceptSenderMessage(await operation.next())
      }
    } catch (error) {
      this.#handlePumpFailure(error)
    }
  }

  async #acceptSenderMessage(message: V2SessionMessage): Promise<void> {
    if (message.kind === V2_MESSAGE_KIND.operationError) {
      const failure = decodeV2OperationErrorControl(message.body)
      if (failure.scope !== 'peer') {
        throw new V2PeerProtocolError('Peer operation received an error from another scope')
      }
      throw new V2SessionSignalingError('Sender rejected peer negotiation')
    }
    if (message.kind === V2_MESSAGE_KIND.peerAnswer) {
      if (this.#answerSeen) throw new V2PeerProtocolError('Sender sent more than one peer answer')
      const answer = decodeV2PeerAnswer(message.body)
      this.#requireBinding(answer)
      this.#answerSeen = true
      this.#controller.enqueue({
        kind: SIGNAL_KIND_ANSWER,
        payload: { type: SIGNAL_KIND_ANSWER, sdp: answer.sdp },
      })
      return
    }
    if (message.kind !== V2_MESSAGE_KIND.peerCandidate) {
      throw new V2PeerProtocolError('Peer operation delivered an unexpected response')
    }
    const candidate = decodeV2PeerCandidate(message.body)
    this.#requireBinding(candidate)
    if (this.#closed) return
    this.#controller.enqueue({
      kind: SIGNAL_KIND_CANDIDATE,
      payload: {
        candidate: candidate.candidate,
        sdpMid: candidate.sdpMid,
        sdpMLineIndex: candidate.sdpMLineIndex,
        usernameFragment: candidate.usernameFragment,
      },
    })
  }

  #handlePumpFailure(error: unknown): void {
    if (this.#closed) return
    if (
      error instanceof V2PeerProtocolError ||
      error instanceof V2SignalingCodecError ||
      error instanceof V2MessageError ||
      (error instanceof V2SessionRuntimeError && error.scope === 'session')
    ) {
      this.#session.close().catch(() => undefined)
      this.#fail(
        new V2SessionRuntimeError(
          'session',
          'Authenticated peer signaling violated its operation binding',
          { cause: error },
        ),
        'session',
      )
      return
    }
    this.#fail(error)
  }

  #requireBinding(candidate: V2PeerBinding): void {
    if (!sameV2PeerBinding(candidate, this.binding)) {
      throw new V2PeerProtocolError('Sender signal changed peer path or attempt identity')
    }
  }

  #fail(reason: unknown, failureScope: 'attempt' | 'session' = 'attempt'): void {
    if (this.#closed) return
    this.#closed = true
    this.#trace({ type: 'route-failed', failureScope, reason })
    const operation = this.#operation
    this.#operation = undefined
    operation?.close()
    try {
      this.#controller.error(reason)
    } catch {
      // The stream consumer may already have released the signaling route.
    }
  }

  #requireOpen(): void {
    if (this.#closed) throw new V2SessionSignalingError('Peer signaling route is closed')
  }

  #trace(event: V2SessionSignalingDecision): void {
    try {
      this.#observe(Object.freeze({
        ...event,
        peerPathId: encodeBase64Url(this.binding.peerPathId),
        attemptId: encodeBase64Url(this.binding.attemptId),
      }) as V2SessionSignalingTrace)
    } catch {
      // Diagnostics cannot own or destabilize authenticated attempt lifecycle.
    }
  }
}

export function createV2PeerBinding(
  randomBytes: (length: number) => Uint8Array = secureRandomBytes,
): V2PeerBinding {
  return Object.freeze({
    peerPathId: nonzeroIdentity(randomBytes),
    attemptId: nonzeroIdentity(randomBytes),
  })
}

function descriptionPayload(payload: unknown, expectedType: 'offer'): string {
  if (
    typeof payload !== 'object' || payload === null ||
    !('type' in payload) || payload.type !== expectedType ||
    !('sdp' in payload) || typeof payload.sdp !== 'string'
  ) {
    throw new V2SessionSignalingError('Peer offer payload is malformed')
  }
  return payload.sdp
}

function candidatePayload(payload: unknown): {
  readonly candidate: string
  readonly sdpMid: string | null
  readonly sdpMLineIndex: number | null
  readonly usernameFragment: string | null
} {
  if (
    typeof payload !== 'object' || payload === null ||
    !('candidate' in payload) || typeof payload.candidate !== 'string'
  ) {
    throw new V2SessionSignalingError('ICE candidate payload is malformed')
  }
  return Object.freeze({
    candidate: payload.candidate,
    sdpMid: optionalStringProperty(payload, 'sdpMid'),
    sdpMLineIndex: optionalNumberProperty(payload, 'sdpMLineIndex'),
    usernameFragment: optionalStringProperty(payload, 'usernameFragment'),
  })
}

function optionalStringProperty(value: object, key: string): string | null {
  if (!(key in value) || value[key as keyof typeof value] === undefined) return null
  const candidate = value[key as keyof typeof value]
  if (candidate !== null && typeof candidate !== 'string') {
    throw new V2SessionSignalingError(`${key} must be text or null`)
  }
  return candidate as string | null
}

function optionalNumberProperty(value: object, key: string): number | null {
  if (!(key in value) || value[key as keyof typeof value] === undefined) return null
  const candidate = value[key as keyof typeof value]
  if (candidate !== null && typeof candidate !== 'number') {
    throw new V2SessionSignalingError(`${key} must be a number or null`)
  }
  return candidate as number | null
}

function snapshotBinding(binding: V2PeerBinding): V2PeerBinding {
  if (
    binding.peerPathId.byteLength !== 16 || binding.attemptId.byteLength !== 16 ||
    !binding.peerPathId.some((item) => item !== 0) ||
    !binding.attemptId.some((item) => item !== 0)
  ) {
    throw new V2SessionSignalingError('Peer path and attempt IDs must be nonzero 16-byte identities')
  }
  return Object.freeze({
    peerPathId: binding.peerPathId.slice(),
    attemptId: binding.attemptId.slice(),
  })
}

function nonzeroIdentity(randomBytes: (length: number) => Uint8Array): Uint8Array<ArrayBuffer> {
  for (let attempt = 0; attempt < 4; attempt += 1) {
    const value = randomBytes(16)
    if (value.byteLength === 16 && value.some((item) => item !== 0)) return value.slice()
  }
  throw new V2SessionSignalingError('Random source did not produce a peer identity')
}

function secureRandomBytes(length: number): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(length)
  globalThis.crypto.getRandomValues(result)
  return result
}
