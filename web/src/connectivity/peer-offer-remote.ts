import { abortReason } from './clock'
import {
  CandidateLimitExceededError,
  PeerNegotiationError,
} from './errors'
import {
  SIGNAL_KIND_ANSWER,
  SIGNAL_KIND_CANDIDATE,
  type ConnectivitySignal,
} from './signaling'

/** Owns answer-before-candidate ordering and the remote candidate lifetime budget. */
export class RemoteNegotiationState {
  readonly #peer: RTCPeerConnection
  readonly #maximumCandidates: number
  #descriptionReceived = false
  #remoteCandidates = 0
  #pendingCandidates: RTCIceCandidateInit[] = []

  constructor(peer: RTCPeerConnection, maximumCandidates: number) {
    this.#peer = peer
    this.#maximumCandidates = maximumCandidates
  }

  get acceptedCandidates(): number {
    return this.#remoteCandidates
  }

  async accept(
    message: ConnectivitySignal,
    signal: AbortSignal,
  ): Promise<'answer' | 'candidate'> {
    if (message.kind === SIGNAL_KIND_CANDIDATE) {
      await this.#acceptCandidate(message.payload, signal)
      return 'candidate'
    }
    if (message.kind !== SIGNAL_KIND_ANSWER || this.#descriptionReceived) {
      throw new PeerNegotiationError(`unexpected signal kind ${JSON.stringify(message.kind)}`)
    }
    await this.#acceptAnswer(message.payload, signal)
    return 'answer'
  }

  async #acceptCandidate(payload: unknown, signal: AbortSignal): Promise<void> {
    this.#remoteCandidates += 1
    if (this.#remoteCandidates > this.#maximumCandidates) {
      throw new CandidateLimitExceededError(this.#maximumCandidates)
    }
    const candidate = decodeCandidate(payload)
    if (!this.#descriptionReceived) {
      this.#pendingCandidates.push(candidate)
      return
    }
    await this.#addCandidate(candidate, signal)
  }

  async #acceptAnswer(payload: unknown, signal: AbortSignal): Promise<void> {
    const answer = decodeAnswer(payload)
    try {
      await awaitWithAbort(this.#peer.setRemoteDescription(answer), signal)
    } catch (cause) {
      if (signal.aborted) {
        throw abortReason(signal)
      }
      throw new PeerNegotiationError('could not set the remote answer', { cause })
    }
    this.#descriptionReceived = true
    for (const candidate of this.#pendingCandidates) {
      await this.#addCandidate(candidate, signal)
    }
    this.#pendingCandidates = []
  }

  async #addCandidate(candidate: RTCIceCandidateInit, signal: AbortSignal): Promise<void> {
    try {
      await awaitWithAbort(this.#peer.addIceCandidate(candidate), signal)
    } catch (cause) {
      if (signal.aborted) {
        throw abortReason(signal)
      }
      throw new PeerNegotiationError('could not add a remote ICE candidate', { cause })
    }
  }
}

export function candidateIdentity(candidate: RTCIceCandidateInit): string {
  return JSON.stringify([
    candidate.candidate,
    candidate.sdpMid ?? null,
    candidate.sdpMLineIndex ?? null,
    candidate.usernameFragment ?? null,
  ])
}

export function awaitWithAbort<T>(operation: Promise<T>, signal: AbortSignal): Promise<T> {
  signal.throwIfAborted()
  return new Promise<T>((resolve, reject) => {
    const aborted = () => reject(abortReason(signal))
    signal.addEventListener('abort', aborted, { once: true })
    operation.then(
      (value) => {
        signal.removeEventListener('abort', aborted)
        resolve(value)
      },
      (reason: unknown) => {
        signal.removeEventListener('abort', aborted)
        reject(reason)
      },
    )
  })
}

function decodeAnswer(payload: unknown): RTCSessionDescriptionInit {
  if (!isRecord(payload) || payload.type !== SIGNAL_KIND_ANSWER ||
      typeof payload.sdp !== 'string' || payload.sdp === '') {
    throw new PeerNegotiationError('answer payload is invalid')
  }
  return { type: 'answer', sdp: payload.sdp }
}

function decodeCandidate(payload: unknown): RTCIceCandidateInit {
  if (!isRecord(payload) || typeof payload.candidate !== 'string' || payload.candidate === '') {
    throw new PeerNegotiationError('ICE candidate payload is invalid')
  }
  return structuredClone(payload) as RTCIceCandidateInit
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
