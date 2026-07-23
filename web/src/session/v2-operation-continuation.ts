import { concatBytes, encodeBase64Url, equalBytes } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import {
  decodeCanonicalCbor,
  requireArray,
  requireBytes,
  requireUnsigned,
} from '../protocol/cbor'
import {
  decodeV2OperationErrorControl,
  type V2OperationErrorControl,
  type V2MessageKind,
  V2_MESSAGE_KIND,
  type V2SessionMessage,
} from './v2-message'
import { V2SessionRuntimeError } from './v2-runtime-types'

export const V2_MAXIMUM_PEER_CANDIDATES = 64

type ContinuationState = 'active' | 'closed' | 'local-cancel' | 'remote-final'

const V2_CONTINUATION_SCHEMA_VERSION = 1n
const V2_OPERATION_ID_BYTES = 16
const V2_PEER_IDENTITY_BYTES = 16
const V2_MAXIMUM_OPERATION_BODY_BYTES = 65_536 - 44

export type V2OperationMessageReservation =
  | { readonly disposition: 'drop' }
  | {
      readonly disposition: 'deliver'
      readonly final: boolean
      accept(): void
      rollback(): void
    }

interface PeerBinding {
  readonly peerPathId: Uint8Array<ArrayBuffer>
  readonly attemptId: Uint8Array<ArrayBuffer>
}

const DROP_RESERVATION = Object.freeze({ disposition: 'drop' as const })

/**
 * Owns every replay-sensitive continuation for one immutable OperationID generation.
 * The active queue and its tombstone share this object so a cross-lane lookup race
 * cannot split candidate, answer, or final history between two ledgers.
 */
export class V2OperationContinuationAuthority {
  readonly requestKind: V2MessageKind
  readonly #peerBinding: PeerBinding | undefined
  readonly #fingerprints = new Map<string, string>()
  readonly #candidateFingerprints = new Set<string>()
  #candidateOverflowFingerprint: string | undefined
  #state: ContinuationState = 'active'
  #tail: Promise<void> = Promise.resolve()

  constructor(requestKind: V2MessageKind, canonicalRequestBody: Uint8Array) {
    this.requestKind = requestKind
    if (requestKind === V2_MESSAGE_KIND.peerOffer) {
      try {
        this.#peerBinding = decodePeerBinding(canonicalRequestBody, 4, 'peer offer')
      } catch (cause) {
        throw new V2SessionRuntimeError(
          'operation',
          'Peer offer request does not establish a canonical continuation binding',
          { cause },
        )
      }
    }
  }

  reserve(message: V2SessionMessage): Promise<V2OperationMessageReservation> {
    return this.#serialize(() => this.#reserve(message))
  }

  acceptLate(message: V2SessionMessage): Promise<void> {
    return this.#serialize(() => this.#acceptLate(message))
  }

  retire(reason: 'local-cancel' | 'remote-final'): void {
    if (this.#state !== 'active') return
    this.#state = reason
    this.#clearCandidateReplayState()
  }

  close(): void {
    this.#state = 'closed'
    this.#clearCandidateReplayState()
    this.#fingerprints.clear()
  }

  async #reserve(message: V2SessionMessage): Promise<V2OperationMessageReservation> {
    if (this.#state === 'closed') return DROP_RESERVATION
    this.#requireAllowed(message)
    this.#validateContinuation(message)
    if (this.#state !== 'active') {
      await this.#acceptLateValidated(message)
      return DROP_RESERVATION
    }
    if (message.kind === V2_MESSAGE_KIND.peerCandidate) {
      return this.#reserveCandidate(message)
    }
    return this.#reserveReplaySlot(message)
  }

  async #reserveCandidate(message: V2SessionMessage): Promise<V2OperationMessageReservation> {
    if (this.#candidateOverflowFingerprint !== undefined) return DROP_RESERVATION
    const fingerprint = await messageFingerprint(message)
    if (this.#state !== 'active') return DROP_RESERVATION
    if (this.#candidateFingerprints.has(fingerprint)) return DROP_RESERVATION
    if (this.#candidateFingerprints.size < V2_MAXIMUM_PEER_CANDIDATES) {
      this.#candidateFingerprints.add(fingerprint)
      return deliverReservation(false, () => this.#candidateFingerprints.delete(fingerprint))
    }
    // One distinct overflow continuation must reach the consumer to produce
    // the attempt-local candidate-limit outcome; every later copy is redundant.
    this.#candidateOverflowFingerprint = fingerprint
    return deliverReservation(false, () => {
      if (this.#candidateOverflowFingerprint === fingerprint) {
        this.#candidateOverflowFingerprint = undefined
      }
    })
  }

  async #reserveReplaySlot(message: V2SessionMessage): Promise<V2OperationMessageReservation> {
    const slot = replaySlot(message.kind)
    if (slot === undefined) return deliverReservation(false)
    const fingerprint = await messageFingerprint(message)
    if (this.#state !== 'active') {
      await this.#acceptLateValidated(message, fingerprint)
      return DROP_RESERVATION
    }
    const existing = this.#fingerprints.get(slot)
    if (existing !== undefined) {
      if (existing === fingerprint) return DROP_RESERVATION
      throw sessionViolation('Operation replay conflicts with authenticated content')
    }
    this.#fingerprints.set(slot, fingerprint)
    return deliverReservation(isFinalResponse(message.kind), () => {
      if (this.#fingerprints.get(slot) === fingerprint) this.#fingerprints.delete(slot)
    })
  }

  async #acceptLate(message: V2SessionMessage): Promise<void> {
    if (this.#state === 'closed') return
    this.#requireAllowed(message)
    this.#validateContinuation(message)
    await this.#acceptLateValidated(message)
  }

  async #acceptLateValidated(
    message: V2SessionMessage,
    knownFingerprint?: string,
  ): Promise<void> {
    if (this.#state === 'closed') return
    if (message.kind === V2_MESSAGE_KIND.peerCandidate && this.#peerBinding !== undefined) return

    const slot = replaySlot(message.kind)
    if (this.#state === 'local-cancel') {
      if (slot !== undefined) await this.#acceptFingerprint(slot, message, true, knownFingerprint)
      return
    }
    if (
      this.#state === 'remote-final' &&
      message.kind === V2_MESSAGE_KIND.peerAnswer &&
      this.#peerBinding !== undefined
    ) {
      await this.#acceptFingerprint('peer-answer', message, true, knownFingerprint)
      return
    }
    if (message.kind === V2_MESSAGE_KIND.blockFragment) return
    if (slot === undefined) {
      throw sessionViolation('Nonterminal traffic arrived for a completed operation')
    }
    await this.#acceptFingerprint(slot, message, false, knownFingerprint)
  }

  async #acceptFingerprint(
    slot: string,
    message: V2SessionMessage,
    allowFirst: boolean,
    knownFingerprint?: string,
  ): Promise<void> {
    const fingerprint = knownFingerprint ?? await messageFingerprint(message)
    if (this.#state === 'closed') return
    const existing = this.#fingerprints.get(slot)
    if (existing === undefined) {
      if (allowFirst) {
        this.#fingerprints.set(slot, fingerprint)
        return
      }
      throw sessionViolation('Nonterminal traffic arrived for a completed operation')
    }
    if (existing !== fingerprint) {
      throw sessionViolation('Late operation traffic conflicts with its continuation authority')
    }
  }

  #requireAllowed(message: V2SessionMessage): void {
    if (isResponseAllowed(this.requestKind, message.kind)) return
    throw sessionViolation(
      `Message kind ${message.kind} cannot answer request kind ${this.requestKind}`,
    )
  }

  #validateContinuation(message: V2SessionMessage): void {
    const expected = this.#peerBinding
    try {
      if (message.kind === V2_MESSAGE_KIND.operationError) {
        const actualScope = decodeV2OperationErrorControl(message.body).scope
        const expectedScope = operationErrorScopeFor(this.requestKind)
        if (expectedScope !== undefined && actualScope !== expectedScope) {
          throw new Error('operation received an error from another scope')
        }
        return
      }
      if (expected === undefined) return
      const actual = decodePeerBinding(
        message.body,
        message.kind === V2_MESSAGE_KIND.peerAnswer ? 4 : 7,
        message.kind === V2_MESSAGE_KIND.peerAnswer ? 'peer answer' : 'peer candidate',
      )
      if (
        !equalBytes(actual.peerPathId, expected.peerPathId) ||
        !equalBytes(actual.attemptId, expected.attemptId)
      ) {
        throw new Error('peer continuation changed path or attempt identity')
      }
    } catch (cause) {
      throw sessionViolation('Authenticated peer continuation violates its offer binding', cause)
    }
  }

  #clearCandidateReplayState(): void {
    this.#candidateFingerprints.clear()
    this.#candidateOverflowFingerprint = undefined
  }

  #serialize<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation)
    this.#tail = result.then(() => undefined, () => undefined)
    return result
  }
}

function deliverReservation(
  final: boolean,
  rollbackEffect: () => void = () => undefined,
): V2OperationMessageReservation {
  let settled = false
  return Object.freeze({
    disposition: 'deliver' as const,
    final,
    accept: () => {
      settled = true
    },
    rollback: () => {
      if (settled) return
      settled = true
      rollbackEffect()
    },
  })
}

function decodePeerBinding(body: Uint8Array, length: number, label: string): PeerBinding {
  const fields = requireArray(
    decodeCanonicalCbor(body, V2_MAXIMUM_OPERATION_BODY_BYTES, label),
    length,
    label,
  )
  if (
    fields.length !== length ||
    requireUnsigned(fields[0], `${label} schema`) !== V2_CONTINUATION_SCHEMA_VERSION
  ) {
    throw new Error(`${label} has a non-canonical continuation prefix`)
  }
  return Object.freeze({
    peerPathId: requireBytes(fields[1], V2_PEER_IDENTITY_BYTES, `${label} peer path ID`, true),
    attemptId: requireBytes(fields[2], V2_PEER_IDENTITY_BYTES, `${label} attempt ID`, true),
  })
}

function replaySlot(kind: V2MessageKind): string | undefined {
  if (isFinalResponse(kind)) return 'final'
  if (kind === V2_MESSAGE_KIND.peerAnswer) return 'peer-answer'
  return undefined
}

function isFinalResponse(kind: V2MessageKind): boolean {
  return kind === V2_MESSAGE_KIND.catalogResult ||
    kind === V2_MESSAGE_KIND.openResults ||
    kind === V2_MESSAGE_KIND.cancel ||
    kind === V2_MESSAGE_KIND.operationError ||
    kind === V2_MESSAGE_KIND.operationComplete ||
    kind === V2_MESSAGE_KIND.leaseResult ||
    kind === V2_MESSAGE_KIND.laneAttach
}

function isResponseAllowed(request: V2MessageKind, response: V2MessageKind): boolean {
  if (response === V2_MESSAGE_KIND.operationError) return true
  switch (request) {
    case V2_MESSAGE_KIND.listChildren:
      return response === V2_MESSAGE_KIND.scanProgress || response === V2_MESSAGE_KIND.catalogResult
    case V2_MESSAGE_KIND.openRevisions:
      return response === V2_MESSAGE_KIND.openResults
    case V2_MESSAGE_KIND.renewLease:
      return response === V2_MESSAGE_KIND.leaseResult
    case V2_MESSAGE_KIND.releaseLease:
      return response === V2_MESSAGE_KIND.operationComplete
    case V2_MESSAGE_KIND.requestBlocks:
      return response === V2_MESSAGE_KIND.blockFragment ||
        response === V2_MESSAGE_KIND.operationComplete
    case V2_MESSAGE_KIND.laneAttach:
      return response === V2_MESSAGE_KIND.laneAttach
    case V2_MESSAGE_KIND.peerOffer:
      return response === V2_MESSAGE_KIND.peerAnswer || response === V2_MESSAGE_KIND.peerCandidate
    default:
      return false
  }
}

function operationErrorScopeFor(
  request: V2MessageKind,
): V2OperationErrorControl['scope'] | undefined {
  switch (request) {
    case V2_MESSAGE_KIND.listChildren:
      return 'directory'
    case V2_MESSAGE_KIND.openRevisions:
    case V2_MESSAGE_KIND.renewLease:
    case V2_MESSAGE_KIND.releaseLease:
      return 'revision'
    case V2_MESSAGE_KIND.requestBlocks:
      return 'block'
    case V2_MESSAGE_KIND.peerOffer:
      return 'peer'
    default:
      return undefined
  }
}

async function messageFingerprint(message: V2SessionMessage): Promise<string> {
  return encodeBase64Url(await sha256(concatBytes([
    Uint8Array.of(message.kind),
    message.operationId ?? new Uint8Array(V2_OPERATION_ID_BYTES),
    message.body,
  ])))
}

function sessionViolation(message: string, cause?: unknown): V2SessionRuntimeError {
  return new V2SessionRuntimeError(
    'session',
    message,
    cause === undefined ? undefined : { cause },
  )
}
