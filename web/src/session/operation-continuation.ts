import { concatBytes, encodeBase64Url, equalBytes } from '../crypto/bytes'
import { requirePeerSlot } from './peer-slots/slots'
import { sha256 } from '../crypto/digest'
import {
  decodeCanonicalCbor,
  requireArray,
  requireBytes,
  requireUnsigned,
} from '../protocol/cbor'
import {
  decodeOperationErrorControl,
  type MessageKind,
  MESSAGE_KIND,
  type SessionMessage,
} from './message'
import { operationErrorScopeAllowed } from './operation-error-contract'
import { SessionRuntimeError } from './runtime-types'

export const MAXIMUM_PEER_CANDIDATES = 64

type ContinuationState = 'active' | 'closed' | 'local-cancel' | 'remote-final'

const CONTINUATION_SCHEMA_VERSION = 4n
const OPERATION_ID_BYTES = 16
const PEER_IDENTITY_BYTES = 16
const MAXIMUM_OPERATION_BODY_BYTES = 65_536 - 44

export type OperationMessageReservation =
  | { readonly disposition: 'drop' }
  | {
      readonly disposition: 'deliver'
      readonly final: boolean
      accept(): void
      rollback(): void
    }

interface PeerBinding {
  readonly purpose: bigint
  readonly peerSlot: number
  readonly peerPathId: Uint8Array<ArrayBuffer>
  readonly attemptId: Uint8Array<ArrayBuffer>
  readonly attemptSequence: bigint
}

/** Retains only local offer authority when per-operation replay detail is collected. */
export class RetiredPeerContinuations {
  readonly #slots = new Map<number, {
    issuedThrough: bigint
    retiredThrough: bigint
    active: Set<bigint>
  }>()

  admit(binding: Readonly<PeerBinding> | undefined): void {
    if (binding === undefined) return
    const key = requirePeerSlot(BigInt(binding.peerSlot))
    let slot = this.#slots.get(key)
    if (slot === undefined) {
      slot = { issuedThrough: 0n, retiredThrough: 0n, active: new Set() }
      this.#slots.set(key, slot)
    }
    if (binding.attemptSequence <= slot.issuedThrough) {
      throw new SessionRuntimeError('operation', 'Peer attempt sequence was reused or regressed')
    }
    slot.issuedThrough = binding.attemptSequence
    slot.active.add(binding.attemptSequence)
  }

  retire(binding: Readonly<PeerBinding> | undefined): void {
    if (binding === undefined) return
    const slot = this.#slots.get(binding.peerSlot)
    if (slot === undefined || !slot.active.delete(binding.attemptSequence)) return
    if (binding.attemptSequence > slot.retiredThrough) slot.retiredThrough = binding.attemptSequence
  }

  drops(message: SessionMessage): boolean {
    if (message.kind !== MESSAGE_KIND.peerAnswer && message.kind !== MESSAGE_KIND.peerCandidate &&
      message.kind !== MESSAGE_KIND.operationError) {
      return false
    }
    try {
      const length = message.kind === MESSAGE_KIND.peerAnswer ? 7 : 10
      const binding = message.kind === MESSAGE_KIND.operationError
        ? decodeOperationErrorControl(message.body).peerAttempt
        : decodePeerBinding(message.body, length, 'retired peer continuation')
      if (binding === undefined) return false
      const slot = this.#slots.get(binding.peerSlot)
      // An incoming body can consult a locally established boundary, never create
      // one. A live sequence still requires its exact operation and attempt binding.
      return slot !== undefined && binding.attemptSequence <= slot.retiredThrough &&
        !slot.active.has(binding.attemptSequence)
    } catch {
      return false
    }
  }

  clear(): void { this.#slots.clear() }
}

const DROP_RESERVATION = Object.freeze({ disposition: 'drop' as const })

/**
 * Owns every replay-sensitive continuation for one immutable OperationID generation.
 * The active queue and its tombstone share this object so a cross-lane lookup race
 * cannot split candidate, answer, or final history between two ledgers.
 */
export class OperationContinuationAuthority {
  readonly requestKind: MessageKind
  readonly #peerBinding: PeerBinding | undefined
  readonly #fingerprints = new Map<string, string>()
  readonly #candidateFingerprints = new Set<string>()
  #candidateOverflowFingerprint: string | undefined
  #state: ContinuationState = 'active'
  #tail: Promise<void> = Promise.resolve()

  constructor(requestKind: MessageKind, canonicalRequestBody: Uint8Array) {
    this.requestKind = requestKind
    if (requestKind === MESSAGE_KIND.peerOffer) {
      try {
        this.#peerBinding = decodePeerBinding(canonicalRequestBody, 7, 'peer offer')
      } catch (cause) {
        throw new SessionRuntimeError(
          'operation',
          'Peer offer request does not establish a canonical continuation binding',
          { cause },
        )
      }
    }
  }

  peerBinding(): Readonly<PeerBinding> | undefined {
    const binding = this.#peerBinding
    return binding === undefined
      ? undefined
      : Object.freeze({
          peerPathId: binding.peerPathId.slice(),
          attemptId: binding.attemptId.slice(),
          attemptSequence: binding.attemptSequence,
          purpose: binding.purpose,
          peerSlot: binding.peerSlot,
        })
  }

  reserve(message: SessionMessage): Promise<OperationMessageReservation> {
    return this.#serialize(() => this.#reserve(message))
  }

  acceptLate(message: SessionMessage): Promise<void> {
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

  async #reserve(message: SessionMessage): Promise<OperationMessageReservation> {
    if (this.#state === 'closed') return DROP_RESERVATION
    this.#requireAllowed(message)
    this.#validateContinuation(message)
    if (this.#state !== 'active') {
      await this.#acceptLateValidated(message)
      return DROP_RESERVATION
    }
    if (message.kind === MESSAGE_KIND.peerCandidate) {
      return this.#reserveCandidate(message)
    }
    return this.#reserveReplaySlot(message)
  }

  async #reserveCandidate(message: SessionMessage): Promise<OperationMessageReservation> {
    if (this.#candidateOverflowFingerprint !== undefined) return DROP_RESERVATION
    const fingerprint = await messageFingerprint(message)
    if (this.#state !== 'active') return DROP_RESERVATION
    if (this.#candidateFingerprints.has(fingerprint)) return DROP_RESERVATION
    if (this.#candidateFingerprints.size < MAXIMUM_PEER_CANDIDATES) {
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

  async #reserveReplaySlot(message: SessionMessage): Promise<OperationMessageReservation> {
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

  async #acceptLate(message: SessionMessage): Promise<void> {
    if (this.#state === 'closed') return
    this.#requireAllowed(message)
    this.#validateContinuation(message)
    await this.#acceptLateValidated(message)
  }

  async #acceptLateValidated(
    message: SessionMessage,
    knownFingerprint?: string,
  ): Promise<void> {
    if (this.#state === 'closed') return
    if (message.kind === MESSAGE_KIND.peerCandidate && this.#peerBinding !== undefined) return

    const slot = replaySlot(message.kind)
    if (this.#state === 'local-cancel') {
      if (slot !== undefined) await this.#acceptFingerprint(slot, message, true, knownFingerprint)
      return
    }
    if (
      this.#state === 'remote-final' &&
      message.kind === MESSAGE_KIND.peerAnswer &&
      this.#peerBinding !== undefined
    ) {
      await this.#acceptFingerprint('peer-answer', message, true, knownFingerprint)
      return
    }
    if (message.kind === MESSAGE_KIND.blockFragment) return
    if (slot === undefined) {
      throw sessionViolation('Nonterminal traffic arrived for a completed operation')
    }
    await this.#acceptFingerprint(slot, message, false, knownFingerprint)
  }

  async #acceptFingerprint(
    slot: string,
    message: SessionMessage,
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

  #requireAllowed(message: SessionMessage): void {
    requireOperationResponse(this.requestKind, message)
  }

  #validateContinuation(message: SessionMessage): void {
    const expected = this.#peerBinding
    try {
      if (message.kind === MESSAGE_KIND.operationError) {
        const failure = decodeOperationErrorControl(message.body)
        if (expected !== undefined && (failure.peerAttempt === undefined ||
          failure.peerAttempt.peerSlot !== expected.peerSlot ||
          failure.peerAttempt.attemptSequence !== expected.attemptSequence ||
          !equalBytes(failure.peerAttempt.peerPathId, expected.peerPathId) ||
          !equalBytes(failure.peerAttempt.attemptId, expected.attemptId))) {
          throw new Error('peer failure changed path or attempt identity')
        }
        return
      }
      if (expected === undefined) return
      const actual = decodePeerBinding(
        message.body,
        message.kind === MESSAGE_KIND.peerAnswer ? 7 : 10,
        message.kind === MESSAGE_KIND.peerAnswer ? 'peer answer' : 'peer candidate',
      )
      if (
        actual.purpose !== expected.purpose ||
        actual.peerSlot !== expected.peerSlot ||
        actual.attemptSequence !== expected.attemptSequence ||
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
): OperationMessageReservation {
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
    decodeCanonicalCbor(body, MAXIMUM_OPERATION_BODY_BYTES, label),
    length,
    label,
  )
  if (
    fields.length !== length ||
    requireUnsigned(fields[0], `${label} schema`) !== CONTINUATION_SCHEMA_VERSION
  ) {
    throw new Error(`${label} has a non-canonical continuation prefix`)
  }
  const purpose = requireUnsigned(fields[5], 'connection purpose')
  if (purpose > 2n) throw new Error('invalid connection purpose')
  const sequence = requireUnsigned(fields[3], 'attempt sequence')
  if (sequence === 0n || sequence > 0xffffffffffffffffn) throw new Error('invalid attempt sequence')
  return Object.freeze({
    attemptSequence: sequence,
    purpose,
    peerSlot: requirePeerSlot(requireUnsigned(fields[4], 'peer slot')),
    peerPathId: requireBytes(fields[1], PEER_IDENTITY_BYTES, `${label} peer path ID`, true),
    attemptId: requireBytes(fields[2], PEER_IDENTITY_BYTES, `${label} attempt ID`, true),
  })
}

function replaySlot(kind: MessageKind): string | undefined {
  if (isFinalResponse(kind)) return 'final'
  if (kind === MESSAGE_KIND.peerAnswer) return 'peer-answer'
  return undefined
}

function isFinalResponse(kind: MessageKind): boolean {
  return kind === MESSAGE_KIND.catalogResult ||
    kind === MESSAGE_KIND.openResults ||
    kind === MESSAGE_KIND.cancel ||
    kind === MESSAGE_KIND.operationError ||
    kind === MESSAGE_KIND.operationComplete ||
    kind === MESSAGE_KIND.leaseResult ||
    kind === MESSAGE_KIND.laneAttach
}

export function requireOperationResponse(requestKind: MessageKind, message: SessionMessage): void {
  if (!isResponseAllowed(requestKind, message.kind)) {
    throw sessionViolation(`Message kind ${message.kind} cannot answer request kind ${requestKind}`)
  }
  if (message.kind !== MESSAGE_KIND.operationError) return
  try {
    if (!operationErrorScopeAllowed(requestKind, decodeOperationErrorControl(message.body).scope)) {
      throw new Error('Operation received an error from another scope')
    }
  } catch (cause) {
    throw sessionViolation('Operation error violates its request contract', cause)
  }
}

function isResponseAllowed(request: MessageKind, response: MessageKind): boolean {
  if (response === MESSAGE_KIND.operationError) return true
  switch (request) {
    case MESSAGE_KIND.listChildren:
      return response === MESSAGE_KIND.scanProgress || response === MESSAGE_KIND.catalogResult
    case MESSAGE_KIND.openRevisions:
      return response === MESSAGE_KIND.openResults
    case MESSAGE_KIND.renewLease:
      return response === MESSAGE_KIND.leaseResult
    case MESSAGE_KIND.releaseLease:
      return response === MESSAGE_KIND.operationComplete
    case MESSAGE_KIND.requestBlocks:
      return response === MESSAGE_KIND.blockFragment ||
        response === MESSAGE_KIND.operationComplete
    case MESSAGE_KIND.laneAttach:
      return response === MESSAGE_KIND.laneAttach
    case MESSAGE_KIND.peerOffer:
      return response === MESSAGE_KIND.peerAnswer || response === MESSAGE_KIND.peerCandidate
    default:
      return false
  }
}

async function messageFingerprint(message: SessionMessage): Promise<string> {
  return encodeBase64Url(await sha256(concatBytes([
    Uint8Array.of(message.kind),
    message.operationId ?? new Uint8Array(OPERATION_ID_BYTES),
    message.body,
  ])))
}

function sessionViolation(message: string, cause?: unknown): SessionRuntimeError {
  return new SessionRuntimeError(
    'session',
    message,
    cause === undefined ? undefined : { cause },
  )
}
