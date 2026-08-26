import type {
  AutomaticCapacityHandoffResult,
  CheckpointAttemptIdentity,
  CheckpointAuthorityObserver,
  CheckpointResourceReleaseReason,
  PreservingWriterCapacityAuthority,
  PreservingWriterCapacityPurpose,
  PreservingWriterCapacityRequest,
  PreservingWriterCapacitySnapshot,
  PreservingWriterCapacityToken,
} from './contracts'
import { snapshotPreservingWriterCost } from './automatic-checkpoint-admission'

const MEBIBYTE_BYTES = 1024n * 1024n
const GIBIBYTE_BYTES = 1024n * MEBIBYTE_BYTES

export const MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES = GIBIBYTE_BYTES

export interface CreatePreservingWriterCapacityAuthorityOptions {
  readonly identity: CheckpointAttemptIdentity
  readonly observe?: CheckpointAuthorityObserver
}

type CapacityTokenState = 'tentative' | 'committed' | 'released'

interface CapacityTokenRecord {
  readonly request: PreservingWriterCapacityRequest
  readonly purpose: PreservingWriterCapacityPurpose
  readonly public: PreservingWriterCapacityToken
  state: CapacityTokenState
}

interface PausedCapacityRequest {
  readonly request: PreservingWriterCapacityRequest
  readonly signal: AbortSignal | undefined
  readonly abort: () => void
  readonly resolve: (token: PreservingWriterCapacityToken) => void
  readonly reject: (reason: unknown) => void
}

export function createPreservingWriterCapacityAuthority(
  options: CreatePreservingWriterCapacityAuthorityOptions,
): PreservingWriterCapacityAuthority {
  return new SharedPreservingWriterCapacityAuthority(options)
}

class SharedPreservingWriterCapacityAuthority implements PreservingWriterCapacityAuthority {
  readonly #identity: CheckpointAttemptIdentity
  readonly #observe: CheckpointAuthorityObserver | undefined
  readonly #knownTokens = new WeakMap<object, CapacityTokenRecord>()
  readonly #activeTokens = new Set<CapacityTokenRecord>()
  readonly #pausedQueue: PausedCapacityRequest[] = []
  #heldTemporaryBytes = 0n
  #accepting = true

  constructor(options: CreatePreservingWriterCapacityAuthorityOptions) {
    this.#identity = snapshotAttemptIdentity(options.identity)
    this.#observe = options.observe
  }

  tryHandoff(
    input: PreservingWriterCapacityRequest,
    current?: PreservingWriterCapacityToken,
  ): AutomaticCapacityHandoffResult {
    const request = snapshotCapacityRequest(input, 'automatic-checkpoint')
    if (!this.#accepting || this.#pausedQueue.length !== 0 ||
        request.cost.temporaryBytes > MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES) {
      this.#emit(request, 'capacity-unavailable')
      return Object.freeze({ kind: 'unavailable', reason: 'capacity-unavailable' })
    }

    const currentRecord = current === undefined ? undefined : this.#requireHandoffToken(current)
    const availableAfterHandoff = MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES -
      this.#heldTemporaryBytes + (currentRecord?.request.cost.temporaryBytes ?? 0n)
    if (request.cost.temporaryBytes > availableAfterHandoff) {
      this.#emit(request, 'capacity-unavailable')
      return Object.freeze({ kind: 'unavailable', reason: 'capacity-unavailable' })
    }

    if (currentRecord !== undefined && !this.#retireToken(currentRecord)) {
      throw settledTokenError('handoff')
    }
    const token = this.#createToken(request, 'automatic-checkpoint')
    if (currentRecord !== undefined) {
      this.#emit(currentRecord.request, 'released', 'automatic-handoff')
    }
    this.#emit(request, 'admitted')
    return Object.freeze({ kind: 'reserved', token })
  }

  reservePaused(
    input: PreservingWriterCapacityRequest,
    signal?: AbortSignal,
  ): Promise<PreservingWriterCapacityToken> {
    let request: PreservingWriterCapacityRequest
    try {
      request = snapshotCapacityRequest(input, 'paused-file-recovery')
      signal?.throwIfAborted()
      if (!this.#accepting) throw capacityClosedError()
    } catch (error) {
      return Promise.reject(error)
    }

    const result = new Promise<PreservingWriterCapacityToken>((resolve, reject) => {
      const queued: PausedCapacityRequest = {
        request,
        signal,
        abort: () => this.#cancelPaused(queued, signal?.reason ?? capacityCancelledError()),
        resolve,
        reject,
      }
      signal?.addEventListener('abort', queued.abort, { once: true })
      this.#pausedQueue.push(queued)
      this.#emit(request, 'paused-recovery-queued')
    })
    this.#pumpPausedQueue()
    return result
  }

  close(reason: CheckpointResourceReleaseReason = 'terminal-drain'): void {
    requireReleaseReason(reason)
    if (!this.#accepting) return
    this.#accepting = false
    const rejection = capacityClosedError()
    for (const queued of this.#pausedQueue.splice(0)) {
      queued.signal?.removeEventListener('abort', queued.abort)
      this.#emit(queued.request, 'released', reason)
      queued.reject(rejection)
    }
    for (const token of [...this.#activeTokens]) this.#releaseToken(token, reason)
  }

  snapshot(): PreservingWriterCapacitySnapshot {
    return Object.freeze({
      accepting: this.#accepting,
      heldTemporaryBytes: this.#heldTemporaryBytes,
      heldTokens: this.#activeTokens.size,
      queuedPausedRecoveries: this.#pausedQueue.length,
      oversizedExclusive: [...this.#activeTokens].some(token =>
        token.request.cost.temporaryBytes >
          MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES),
    })
  }

  #pumpPausedQueue(): void {
    if (!this.#accepting) return
    while (this.#pausedQueue.length !== 0) {
      const queued = this.#pausedQueue[0]!
      if (!this.#canReservePaused(queued.request.cost.temporaryBytes)) return
      this.#pausedQueue.shift()
      queued.signal?.removeEventListener('abort', queued.abort)
      const token = this.#createToken(queued.request, 'paused-file-recovery')
      this.#emit(queued.request, 'paused-recovery-admitted')
      queued.resolve(token)
      if (queued.request.cost.temporaryBytes >
          MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES) return
    }
  }

  #canReservePaused(temporaryBytes: bigint): boolean {
    if (temporaryBytes > MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES) {
      return this.#activeTokens.size === 0
    }
    const hasOversizedReservation = [...this.#activeTokens].some(token =>
      token.request.cost.temporaryBytes >
        MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES)
    return !hasOversizedReservation &&
      this.#heldTemporaryBytes + temporaryBytes <=
        MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES
  }

  #createToken(
    request: PreservingWriterCapacityRequest,
    purpose: PreservingWriterCapacityPurpose,
  ): PreservingWriterCapacityToken {
    const token = {} as PreservingWriterCapacityToken
    const record: CapacityTokenRecord = {
      request,
      purpose,
      public: token,
      state: 'tentative',
    }
    Object.assign(token, {
      purpose,
      reservedTemporaryBytes: request.cost.temporaryBytes,
      commit: () => this.#commitToken(record),
      release: (reason: CheckpointResourceReleaseReason) => this.#releaseToken(record, reason),
    })
    Object.freeze(token)
    this.#knownTokens.set(token, record)
    this.#activeTokens.add(record)
    this.#heldTemporaryBytes += request.cost.temporaryBytes
    return token
  }

  #commitToken(token: CapacityTokenRecord): void {
    if (token.state === 'committed') return
    if (token.state !== 'tentative') throw settledTokenError('commit')
    token.state = 'committed'
    this.#emit(token.request, 'committed')
  }

  #releaseToken(token: CapacityTokenRecord, reason: CheckpointResourceReleaseReason): void {
    requireReleaseReason(reason)
    if (token.state === 'released') return
    if (!this.#retireToken(token)) return
    this.#emit(token.request, 'released', reason)
    this.#pumpPausedQueue()
  }

  #retireToken(token: CapacityTokenRecord): boolean {
    token.state = 'released'
    if (!this.#activeTokens.delete(token)) return false
    this.#heldTemporaryBytes -= token.request.cost.temporaryBytes
    return true
  }

  #requireHandoffToken(token: PreservingWriterCapacityToken): CapacityTokenRecord {
    const record = this.#knownTokens.get(token as object)
    if (record === undefined) {
      throw new TypeError('Preserving writer capacity token belongs to another authority')
    }
    if (record.state !== 'committed' || !this.#activeTokens.has(record)) {
      throw settledTokenError('handoff')
    }
    return record
  }

  #cancelPaused(queued: PausedCapacityRequest, reason: unknown): void {
    const index = this.#pausedQueue.indexOf(queued)
    if (index < 0) return
    this.#pausedQueue.splice(index, 1)
    queued.signal?.removeEventListener('abort', queued.abort)
    this.#emit(queued.request, 'released', 'cancelled')
    queued.reject(reason)
    this.#pumpPausedQueue()
  }

  #emit(
    request: PreservingWriterCapacityRequest,
    decision: Parameters<CheckpointAuthorityObserver>[0]['decision'],
    releaseReason?: CheckpointResourceReleaseReason,
  ): void {
    if (this.#observe === undefined) return
    try {
      this.#observe(Object.freeze({
        authority: 'preserving-capacity',
        ...this.#identity,
        materializationRelativePath: request.materializationRelativePath,
        trigger: request.trigger,
        ...(request.checkpointOrdinal === undefined
          ? {}
          : { checkpointOrdinal: request.checkpointOrdinal }),
        cost: request.cost,
        ...(request.remainingAutomaticWriteAmplificationBytes === undefined
          ? {}
          : {
              remainingAutomaticWriteAmplificationBytes:
                request.remainingAutomaticWriteAmplificationBytes,
            }),
        decision,
        ...(releaseReason === undefined ? {} : { releaseReason }),
      }))
    } catch {
      // Capacity must remain authoritative when an observation sink fails.
    }
  }
}

function snapshotCapacityRequest(
  input: PreservingWriterCapacityRequest,
  purpose: PreservingWriterCapacityPurpose,
): PreservingWriterCapacityRequest {
  const expectedTrigger = purpose === 'automatic-checkpoint'
    ? ['pending-bytes', 'pending-time']
    : ['paused-file-recovery']
  if (!expectedTrigger.includes(input?.trigger)) {
    throw new TypeError(`Preserving writer ${purpose} trigger is invalid`)
  }
  if (input.checkpointOrdinal !== undefined &&
      (!Number.isSafeInteger(input.checkpointOrdinal) || input.checkpointOrdinal <= 0)) {
    throw new TypeError('Preserving writer checkpoint ordinal is invalid')
  }
  if (input.remainingAutomaticWriteAmplificationBytes !== undefined &&
      (typeof input.remainingAutomaticWriteAmplificationBytes !== 'bigint' ||
       input.remainingAutomaticWriteAmplificationBytes < 0n)) {
    throw new TypeError('Remaining automatic write amplification bytes must not be negative')
  }
  const path = snapshotPath(input.materializationRelativePath)
  return Object.freeze({
    materializationRelativePath: path,
    trigger: input.trigger,
    ...(input.checkpointOrdinal === undefined ? {} : { checkpointOrdinal: input.checkpointOrdinal }),
    cost: snapshotPreservingWriterCost(input.cost),
    ...(input.remainingAutomaticWriteAmplificationBytes === undefined
      ? {}
      : {
          remainingAutomaticWriteAmplificationBytes:
            input.remainingAutomaticWriteAmplificationBytes,
        }),
  })
}

function snapshotAttemptIdentity(identity: CheckpointAttemptIdentity): CheckpointAttemptIdentity {
  const receiveOperationId = requireIdentity(identity?.receiveOperationId, 'receive operation')
  const transferJobId = requireIdentity(identity?.transferJobId, 'transfer job')
  const outputSessionId = requireIdentity(identity?.outputSessionId, 'output session')
  return Object.freeze({ receiveOperationId, transferJobId, outputSessionId })
}

function snapshotPath(path: readonly string[]): readonly string[] {
  if (!Array.isArray(path) || path.some(part => typeof part !== 'string' || part.length === 0)) {
    throw new TypeError('Checkpoint materialization path is invalid')
  }
  return Object.freeze([...path])
}

function requireIdentity(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`Checkpoint ${label} identity is required`)
  }
  return value
}

function requireReleaseReason(reason: CheckpointResourceReleaseReason): void {
  if (typeof reason !== 'string' || reason.length === 0) {
    throw new TypeError('Checkpoint resource release reason is required')
  }
}

function settledTokenError(action: string): DOMException {
  return new DOMException(
    `Preserving writer capacity token cannot ${action} after settlement`,
    'InvalidStateError',
  )
}

function capacityClosedError(): DOMException {
  return new DOMException('Preserving writer capacity authority is closed', 'InvalidStateError')
}

function capacityCancelledError(): DOMException {
  return new DOMException('Paused preserving writer capacity request was cancelled', 'AbortError')
}
