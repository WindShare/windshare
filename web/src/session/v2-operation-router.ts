import { encodeBase64Url } from '../crypto/bytes'
import {
  createProtocolFailure,
  type FailureCorrelation,
  type ProtocolFailure,
} from '../diagnostics/incident/fact'
import {
  protocolMessageKindV1,
  type V2ProtocolOperationSettlement,
  type V2ProtocolTraceEvent,
  type V2ProtocolTraceSource,
} from './v2-diagnostics'
import {
  createV2PeerAttemptIdentity,
  createV2PeerPathIdentityValue,
  createV2ProtocolOperationIdentity,
  type V2ProtocolSessionIdentity,
} from './v2-identities'
import {
  decodeV2OperationErrorControl,
  peerFailureScope,
  type V2MessageKind,
  V2_MESSAGE_KIND,
  type V2SessionMessage,
} from './v2-message'
import {
  V2OperationContinuationAuthority,
  V2RetiredPeerContinuations,
} from './v2-operation-continuation'
import { type V2SessionOperation, V2SessionRuntimeError } from './v2-runtime-types'

export const V2_SESSION_OPERATION_RESPONSE_QUEUE = 512
export const V2_SESSION_CONTROL_BACKLOG = 256
export const V2_SESSION_DATA_BACKLOG = 1_024
export const V2_SESSION_PLAINTEXT_BUDGET_BYTES = 128 * 1024 * 1024
export const V2_SESSION_BLOCK_CACHE_RESERVATION_BYTES = 64 * 1024 * 1024
export const V2_SESSION_REASSEMBLY_RESERVATION_BYTES = 8 * 4 * 1024 * 1024
// Queue admission receives the remainder after the same session's cache and
// bounded upstream reassemblies reserve their worst-case plaintext ownership.
export const V2_SESSION_PLAINTEXT_BACKLOG_BYTES = V2_SESSION_PLAINTEXT_BUDGET_BYTES -
  V2_SESSION_BLOCK_CACHE_RESERVATION_BYTES - V2_SESSION_REASSEMBLY_RESERVATION_BYTES
export const V2_MAXIMUM_ACTIVE_OPERATIONS = 256
export const V2_OPERATION_TOMBSTONE_MILLISECONDS = 30_000
export const V2_MAXIMUM_OPERATION_TOMBSTONES = 4_096

interface PendingRead {
  resolve(message: V2SessionMessage): void
  reject(reason: unknown): void
}

export class V2OperationQueue implements V2SessionOperation {
  readonly id: Uint8Array<ArrayBuffer>
  readonly requestKind: V2MessageKind
  readonly #messages: V2SessionMessage[] = []
  readonly #readers: PendingRead[] = []
  readonly #settlementCleanups = new Set<() => void>()
  readonly #admission: V2SessionQueueAdmission
  readonly #authority: V2OperationContinuationAuthority
  readonly #onClose: (
    authority: V2OperationContinuationAuthority,
    settlement: V2ProtocolOperationSettlement,
  ) => void
  readonly #onConsumerSettled: () => void
  readonly #onAuthenticatedMessage: (
    message: V2SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ) => void
  #pushTail: Promise<void> = Promise.resolve()
  #failure: unknown
  #settlement: V2ProtocolOperationSettlement | undefined
  #closed = false
  #consumerSettled = false

  constructor(
    id: Uint8Array,
    requestKind: V2MessageKind,
    canonicalRequestBody: Uint8Array,
    admission: V2SessionQueueAdmission,
    onClose: (
      authority: V2OperationContinuationAuthority,
      settlement: V2ProtocolOperationSettlement,
    ) => void,
    onConsumerSettled: () => void,
    onAuthenticatedMessage: (
      message: V2SessionMessage,
      laneId?: number,
      laneEpoch?: number,
    ) => void,
  ) {
    this.id = id.slice()
    this.requestKind = requestKind
    this.#admission = admission
    this.#authority = new V2OperationContinuationAuthority(requestKind, canonicalRequestBody)
    this.#onClose = onClose
    this.#onConsumerSettled = onConsumerSettled
    this.#onAuthenticatedMessage = onAuthenticatedMessage
  }

  peerBinding(): Readonly<{
    peerPathId: Uint8Array<ArrayBuffer>
    attemptId: Uint8Array<ArrayBuffer>
    attemptSequence: bigint
  }> | undefined {
    return this.#authority.peerBinding()
  }

  get settlement(): V2ProtocolOperationSettlement | undefined {
    return this.#settlement
  }

  next(signal?: AbortSignal): Promise<V2SessionMessage> {
    signal?.throwIfAborted()
    const available = this.#messages.shift()
    if (available !== undefined) {
      this.#admission.release(available)
      if (this.#closed && this.#messages.length === 0) this.#settleConsumer()
      return Promise.resolve(available)
    }
    if (this.#failure !== undefined) return Promise.reject(this.#failure)
    if (this.#closed) {
      return Promise.reject(new V2SessionRuntimeError('operation', 'Operation is complete'))
    }
    return new Promise<V2SessionMessage>((resolve, reject) => {
      const clear = () => signal?.removeEventListener('abort', abort)
      const abort = () => {
        const index = this.#readers.indexOf(pending)
        if (index >= 0) this.#readers.splice(index, 1)
        clear()
        reject(signal?.reason ?? new DOMException('Operation aborted', 'AbortError'))
      }
      const pending: PendingRead = {
        resolve: (value) => {
          clear()
          resolve(value)
        },
        reject: (reason) => {
          clear()
          reject(reason)
        },
      }
      this.#readers.push(pending)
      signal?.addEventListener('abort', abort, { once: true })
    })
  }

  push(message: V2SessionMessage, laneId?: number, laneEpoch?: number): Promise<void> {
    const pushed = this.#pushTail.then(() => this.#push(message, laneId, laneEpoch))
    this.#pushTail = pushed.catch(() => undefined)
    return pushed
  }

  async #push(
    message: V2SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ): Promise<void> {
    if (this.#failure !== undefined) return
    if (this.#closed) {
      await this.#authority.acceptLate(message)
      return
    }
    const reservation = await this.#authority.reserve(message)
    if (reservation.disposition === 'drop') return
    if (this.#closed || this.#failure !== undefined) {
      reservation.rollback()
      if (this.#failure === undefined) await this.#authority.acceptLate(message)
      return
    }
    try {
      this.#onAuthenticatedMessage(message, laneId, laneEpoch)
      const reader = this.#readers.shift()
      if (reader === undefined) {
        if (this.#messages.length >= V2_SESSION_OPERATION_RESPONSE_QUEUE) {
          throw new V2SessionRuntimeError('session', 'Operation response queue is full')
        }
        this.#admission.charge(message)
        this.#messages.push(message)
      } else {
        reader.resolve(message)
      }
      reservation.accept()
      if (reservation.final) this.#finish(false)
    } catch (error) {
      reservation.rollback()
      throw error
    }
  }

  fail(reason: unknown): void {
    if (this.#failure !== undefined) return
    const settlement = isSessionFailure(reason) ? 'session_terminal' : 'local_cancel'
    if (settlement === 'session_terminal') this.#authority.close()
    else this.#authority.retire('local-cancel')
    this.#rejectConsumer(reason, settlement)
  }

  cancel(cause: unknown): void {
    if (this.#failure !== undefined) return
    this.#authority.retire('local-cancel')
    this.#rejectConsumer(cause, 'local_cancel')
  }

  #rejectConsumer(reason: unknown, settlement: V2ProtocolOperationSettlement): void {
    if (this.#closed) {
      this.#failure = reason
      this.#clearMessages()
      for (const reader of this.#readers.splice(0)) reader.reject(reason)
      this.#settleConsumer()
      return
    }
    this.#failure = reason
    this.#closed = true
    this.#clearMessages()
    for (const reader of this.#readers.splice(0)) reader.reject(reason)
    this.#settle(true, settlement)
  }

  close(): void {
    if (this.#closed) {
      this.#clearMessages()
      this.#settleConsumer()
      return
    }
    this.#finish(true)
  }

  onSettled(cleanup: () => void): void {
    if (this.#consumerSettled) {
      cleanup()
      return
    }
    this.#settlementCleanups.add(cleanup)
  }

  #finish(acceptLateNonterminal: boolean): void {
    if (this.#closed) return
    this.#closed = true
    this.#authority.retire(acceptLateNonterminal ? 'local-cancel' : 'remote-final')
    if (acceptLateNonterminal) this.#clearMessages()
    for (const reader of this.#readers.splice(0)) {
      reader.reject(new V2SessionRuntimeError('operation', 'Operation is complete'))
    }
    this.#settle(
      acceptLateNonterminal,
      acceptLateNonterminal ? 'local_cancel' : 'remote_final',
    )
  }

  #settle(
    acceptLateNonterminal: boolean,
    settlement: V2ProtocolOperationSettlement,
  ): void {
    this.#settlement = settlement
    this.#onClose(this.#authority, settlement)
    if (acceptLateNonterminal || this.#messages.length === 0) this.#settleConsumer()
  }

  #clearMessages(): void {
    for (const message of this.#messages.splice(0)) this.#admission.release(message)
  }

  #settleConsumer(): void {
    if (this.#consumerSettled) return
    this.#consumerSettled = true
    for (const cleanup of this.#settlementCleanups) cleanup()
    this.#settlementCleanups.clear()
    this.#onConsumerSettled()
  }

}

export interface V2OperationRouterDiagnostics {
  readonly protocolSessionIdentity: V2ProtocolSessionIdentity
  readonly trace?: V2ProtocolTraceSource
}

export class V2OperationRouter {
  readonly #operations = new Map<string, V2OperationQueue>()
  readonly #draining = new Set<V2OperationQueue>()
  readonly #onTerminal: (reason: unknown) => void
  readonly #now: () => number
  readonly #tombstones = new Map<string, V2OperationTombstone>()
  readonly #retiredPeers = new V2RetiredPeerContinuations()
  readonly #admission = new V2SessionQueueAdmission()
  readonly #diagnostics: V2OperationRouterDiagnostics | undefined
  readonly #pathControls = new Set<(body: Uint8Array<ArrayBuffer>) => void>()
  readonly #protocolFailures = new WeakMap<V2SessionMessage, ProtocolFailure>()
  #terminal: unknown

  constructor(
    onTerminal: (reason: unknown) => void,
    now: () => number = () => Date.now(),
    diagnostics?: V2OperationRouterDiagnostics,
  ) {
    this.#onTerminal = onTerminal
    this.#now = now
    this.#diagnostics = diagnostics
  }

  create(
    id: Uint8Array,
    requestKind: V2MessageKind,
    canonicalRequestBody: Uint8Array,
  ): V2OperationQueue {
    if (this.#terminal !== undefined) {
      throw new V2SessionRuntimeError('session', 'Protocol session is terminal')
    }
    this.#pruneTombstones()
    const key = encodeBase64Url(id)
    if (this.#operations.has(key) || this.#tombstones.has(key)) {
      throw new V2SessionRuntimeError('operation', 'Operation ID was reused')
    }
    if (this.#operations.size >= V2_MAXIMUM_ACTIVE_OPERATIONS) {
      throw new V2SessionRuntimeError('session', 'Active operation budget is exhausted')
    }
    if (this.#operations.size + this.#tombstones.size >= V2_MAXIMUM_OPERATION_TOMBSTONES) {
      throw new V2SessionRuntimeError('session', 'Operation tombstone budget is exhausted')
    }
    const operation = new V2OperationQueue(
      id,
      requestKind,
      canonicalRequestBody,
      this.#admission,
      (authority, settlement) => {
        this.#retiredPeers.retire(authority.peerBinding())
        this.#operations.delete(key)
        this.#draining.add(operation)
        this.#tombstones.set(key, new V2OperationTombstone(
          this.#now() + V2_OPERATION_TOMBSTONE_MILLISECONDS,
          authority,
        ))
        if (settlement !== 'remote_final') {
          this.#emitSettlement(operation, settlement)
        }
      },
      () => this.#draining.delete(operation),
      (message, laneId, laneEpoch) => {
        this.#captureProtocolFailure(operation, message, laneId, laneEpoch)
        if (message.kind === V2_MESSAGE_KIND.operationError) {
          const failure = decodeV2OperationErrorControl(message.body)
          if (failure.scope === 'peer' && peerFailureScope(failure.code) === 'session-terminal') {
            const reason = new V2SessionRuntimeError('session', 'Authenticated peer failure ended the session')
            this.terminate(reason)
            this.#onTerminal(reason)
            throw reason
          }
        }
      },
    )
    this.#retiredPeers.admit(operation.peerBinding())
    this.#operations.set(key, operation)
    return operation
  }

  owns(operation: V2SessionOperation): operation is V2OperationQueue {
    return this.#operations.get(encodeBase64Url(operation.id)) === operation
  }

  active(): readonly V2OperationQueue[] {
    return Object.freeze([...this.#operations.values()])
  }

  protocolFailureFor(message: V2SessionMessage): ProtocolFailure | undefined {
    return this.#protocolFailures.get(message)
  }

  subscribePeerPathControls(listener: (body: Uint8Array<ArrayBuffer>) => void): () => void {
    this.#pathControls.add(listener)
    return () => this.#pathControls.delete(listener)
  }

  async route(
    message: V2SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ): Promise<void> {
    if (this.#terminal !== undefined) return
    if (message.kind === V2_MESSAGE_KIND.sessionTerminal) {
      const reason = new V2SessionRuntimeError('session', 'Sender ended the protocol session')
      this.terminate(reason)
      this.#onTerminal(reason)
      return
    }
    if (message.kind === V2_MESSAGE_KIND.peerPathControl) {
      for (const listener of this.#pathControls) listener(message.body.slice())
      return
    }
    const operationId = message.operationId
    if (operationId === undefined) {
      throw new V2SessionRuntimeError('session', 'Inbound operation message has no identity')
    }
    const key = encodeBase64Url(operationId)
    const operation = this.#operations.get(key)
    if (operation !== undefined) {
      await operation.push(message, laneId, laneEpoch)
      if (message.kind !== V2_MESSAGE_KIND.blockFragment) {
        this.#emitTrace(() => Object.freeze({
          eventName: 'protocol_operation',
          transition: 'response_received',
          requestKind: protocolMessageKindV1(operation.requestKind),
          responseKind: protocolMessageKindV1(message.kind),
          correlation: this.#operationCorrelation(operation, laneId, laneEpoch),
        }))
      }
      const protocolFailure = this.#protocolFailures.get(message)
      if (protocolFailure !== undefined) {
        this.#emitTrace(() => Object.freeze({
          eventName: 'protocol_operation',
          transition: 'authenticated_failure',
          requestKind: protocolFailure.requestKind,
          protocolFailure,
          correlation: protocolFailure.correlation,
        }))
      }
      if (operation.settlement === 'remote_final') {
        this.#emitSettlement(operation, 'remote_final', laneId, laneEpoch)
      }
      return
    }
    await this.#routeRetired(key, message)
  }

  async #routeRetired(key: string, message: V2SessionMessage): Promise<void> {
    this.#pruneTombstones()
    const tombstone = this.#tombstones.get(key)
    if (tombstone === undefined) {
      if (this.#retiredPeers.drops(message)) return
      throw new V2SessionRuntimeError('session', 'Inbound message uses an unknown operation ID')
    }
    await tombstone.accept(message)
  }

  terminate(reason: unknown): void {
    if (this.#terminal !== undefined) return
    this.#terminal = reason
    for (const operation of [...this.#operations.values()]) operation.fail(reason)
    for (const operation of [...this.#draining]) operation.fail(reason)
    for (const tombstone of this.#tombstones.values()) tombstone.close()
    this.#operations.clear()
    this.#draining.clear()
    this.#tombstones.clear()
    this.#retiredPeers.clear()
  }

  #captureProtocolFailure(
    operation: V2OperationQueue,
    message: V2SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ): void {
    if (message.kind !== V2_MESSAGE_KIND.operationError) return
    const diagnostics = this.#diagnostics
    if (diagnostics === undefined) return
    const decoded = decodeV2OperationErrorControl(message.body)
    const peerBinding = operation.peerBinding()
    const correlation: ProtocolFailure['correlation'] = Object.freeze({
      protocolSessionId: diagnostics.protocolSessionIdentity,
      protocolOperationId: createV2ProtocolOperationIdentity(operation.id),
      ...(peerBinding === undefined
        ? {}
        : {
            peerPathId: createV2PeerPathIdentityValue(peerBinding.peerPathId),
            peerAttemptId: createV2PeerAttemptIdentity(peerBinding.attemptId),
          }),
      ...(laneId === undefined || laneEpoch === undefined
        ? {}
        : { lane: Object.freeze({ id: laneId, epoch: laneEpoch }) }),
    })
    this.#protocolFailures.set(message, createProtocolFailure({
      requestKind: protocolMessageKindV1(operation.requestKind),
      wireScope: decoded.scope,
      wireCode: decoded.code,
      retryable: decoded.retryable,
      ...(decoded.retryAfterMilliseconds === undefined
        ? {}
        : { retryAfterMilliseconds: decoded.retryAfterMilliseconds }),
      settlement: Object.freeze({ kind: 'received_authenticated' }),
      correlation,
    }))
  }

  #emitSettlement(
    operation: V2OperationQueue,
    settlement: V2ProtocolOperationSettlement,
    laneId?: number,
    laneEpoch?: number,
  ): void {
    this.#emitTrace(() => Object.freeze({
      eventName: 'protocol_operation',
      transition: 'settled',
      requestKind: protocolMessageKindV1(operation.requestKind),
      settlement,
      correlation: this.#operationCorrelation(operation, laneId, laneEpoch),
    }))
  }

  #operationCorrelation(
    operation: V2OperationQueue,
    laneId: number | undefined,
    laneEpoch: number | undefined,
  ): FailureCorrelation {
    const diagnostics = this.#diagnostics
    if (diagnostics === undefined) {
      throw new V2SessionRuntimeError('session', 'Protocol trace correlation is unavailable')
    }
    const peerBinding = operation.peerBinding()
    return Object.freeze({
      protocolSessionId: diagnostics.protocolSessionIdentity,
      protocolOperationId: createV2ProtocolOperationIdentity(operation.id),
      ...(peerBinding === undefined
        ? {}
        : {
            peerPathId: createV2PeerPathIdentityValue(peerBinding.peerPathId),
            peerAttemptId: createV2PeerAttemptIdentity(peerBinding.attemptId),
          }),
      ...(laneId === undefined || laneEpoch === undefined
        ? {}
        : { lane: Object.freeze({ id: laneId, epoch: laneEpoch }) }),
    })
  }

  #emitTrace(createEvent: () => V2ProtocolTraceEvent): void {
    try {
      const observer = this.#diagnostics?.trace?.current
      if (observer === undefined) return
      observer(createEvent())
    } catch {
      // Trace failure cannot alter authenticated routing or queue settlement.
    }
  }

  #pruneTombstones(): void {
    const now = this.#now()
    for (const [key, tombstone] of this.#tombstones) {
      if (tombstone.expiresAt <= now) {
        tombstone.close()
        this.#tombstones.delete(key)
      }
    }
  }
}

class V2SessionQueueAdmission {
  #control = 0
  #data = 0
  #plaintextBytes = 0

  charge(message: V2SessionMessage): void {
    const nextControl = this.#control + (message.data ? 0 : 1)
    const nextData = this.#data + (message.data ? 1 : 0)
    const nextBytes = this.#plaintextBytes + message.plaintext.byteLength
    if (
      nextControl > V2_SESSION_CONTROL_BACKLOG ||
      nextData > V2_SESSION_DATA_BACKLOG ||
      nextBytes > V2_SESSION_PLAINTEXT_BACKLOG_BYTES
    ) {
      throw new V2SessionRuntimeError('session', 'Protocol session response backlog is full')
    }
    this.#control = nextControl
    this.#data = nextData
    this.#plaintextBytes = nextBytes
  }

  release(message: V2SessionMessage): void {
    if (message.data) this.#data -= 1
    else this.#control -= 1
    this.#plaintextBytes -= message.plaintext.byteLength
    if (this.#control < 0 || this.#data < 0 || this.#plaintextBytes < 0) {
      throw new V2SessionRuntimeError('session', 'Protocol session response admission underflowed')
    }
  }
}

class V2OperationTombstone {
  readonly expiresAt: number
  readonly #authority: V2OperationContinuationAuthority

  constructor(
    expiresAt: number,
    authority: V2OperationContinuationAuthority,
  ) {
    this.expiresAt = expiresAt
    this.#authority = authority
  }

  accept(message: V2SessionMessage): Promise<void> {
    return this.#authority.acceptLate(message)
  }

  close(): void {
    this.#authority.close()
  }
}

function isSessionFailure(reason: unknown): boolean {
  return reason instanceof V2SessionRuntimeError && reason.scope === 'session'
}
