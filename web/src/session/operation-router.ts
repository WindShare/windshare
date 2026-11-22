import {
  inferredRetiredRequestKind,
  OperationTombstone,
  requireRequestKind,
  type ProtocolTraceContext,
} from './operation-retirement'
import { cancellationTraceReason, snapshotOperationRequest, type CancellationTraceReason } from './operation-diagnostics'
import {
  createReceivedProtocolError,
  protocolMessageKindV1,
  type ProtocolOperationSettlement,
  type ProtocolTraceEvent,
  type ReceivedProtocolError,
} from './diagnostics'
import {
  createPeerAttemptIdentity,
  createPeerPathIdentityValue,
  createProtocolOperationIdentity,
  type ProtocolCorrelation,
} from './identities'
import {
  decodeOperationErrorControl,
  peerFailureScope,
  type MessageKind,
  MESSAGE_KIND,
  type SessionMessage,
} from './message'
import {
  OperationContinuationAuthority,
  RetiredPeerContinuations,
} from './operation-continuation'
import { type OperationCancelReason, type SessionOperation, SessionRuntimeError } from './runtime-types'

export const SESSION_OPERATION_RESPONSE_QUEUE = 512
export const SESSION_CONTROL_BACKLOG = 256
export const SESSION_DATA_BACKLOG = 1_024
export const SESSION_PLAINTEXT_BUDGET_BYTES = 128 * 1024 * 1024
export const SESSION_BLOCK_CACHE_RESERVATION_BYTES = 64 * 1024 * 1024
export const SESSION_REASSEMBLY_RESERVATION_BYTES = 8 * 4 * 1024 * 1024
// Queue admission receives the remainder after the same session's cache and
// bounded upstream reassemblies reserve their worst-case plaintext ownership.
export const SESSION_PLAINTEXT_BACKLOG_BYTES = SESSION_PLAINTEXT_BUDGET_BYTES -
  SESSION_BLOCK_CACHE_RESERVATION_BYTES - SESSION_REASSEMBLY_RESERVATION_BYTES
export const MAXIMUM_ACTIVE_OPERATIONS = 256
export const MAXIMUM_RECENT_TOMBSTONES = 4_096

const MAXIMUM_OPERATION_ID = 0xffff_ffff_ffff_ffffn

interface PendingRead {
  resolve(message: SessionMessage): void
  reject(reason: unknown): void
}

export class OperationQueue implements SessionOperation {
  readonly id: bigint
  readonly requestKind: MessageKind
  readonly requestTrace: ReturnType<typeof snapshotOperationRequest>
  #cancellationReason: CancellationTraceReason | undefined
  readonly #messages: SessionMessage[] = []
  readonly #readers: PendingRead[] = []
  readonly #settlementCleanups = new Set<() => void>()
  readonly #admission: SessionQueueAdmission
  readonly #authority: OperationContinuationAuthority
  readonly #onClose: (
    authority: OperationContinuationAuthority,
    settlement: ProtocolOperationSettlement,
  ) => void
  readonly #onConsumerSettled: () => void
  readonly #onAuthenticatedMessage: (
    message: SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ) => void
  #pushTail: Promise<void> = Promise.resolve()
  #failure: unknown
  #settlement: ProtocolOperationSettlement | undefined
  #closed = false
  #consumerSettled = false

  constructor(
    id: bigint,
    requestKind: MessageKind,
    canonicalRequestBody: Uint8Array,
    admission: SessionQueueAdmission,
    onClose: (
      authority: OperationContinuationAuthority,
      settlement: ProtocolOperationSettlement,
    ) => void,
    onConsumerSettled: () => void,
    onAuthenticatedMessage: (
      message: SessionMessage,
      laneId?: number,
      laneEpoch?: number,
    ) => void,
    requestTrace?: ReturnType<typeof snapshotOperationRequest>,
  ) {
    this.id = id
    this.requestKind = requestKind
    this.requestTrace = requestTrace
    this.#admission = admission
    this.#authority = new OperationContinuationAuthority(requestKind, canonicalRequestBody)
    this.#onClose = onClose
    this.#onConsumerSettled = onConsumerSettled
    this.#onAuthenticatedMessage = onAuthenticatedMessage
  }

  peerBinding(): Readonly<{
    peerSlot: number
    peerPathId: Uint8Array<ArrayBuffer>
    attemptId: Uint8Array<ArrayBuffer>
    attemptSequence: bigint
    purpose: bigint
  }> | undefined {
    return this.#authority.peerBinding()
  }

  get cancellationReason(): CancellationTraceReason | undefined { return this.#cancellationReason }

  get settlement(): ProtocolOperationSettlement | undefined {
    return this.#settlement
  }

  next(signal?: AbortSignal): Promise<SessionMessage> {
    signal?.throwIfAborted()
    const available = this.#messages.shift()
    if (available !== undefined) {
      this.#admission.release(available)
      if (this.#closed && this.#messages.length === 0) this.#settleConsumer()
      return Promise.resolve(available)
    }
    if (this.#failure !== undefined) return Promise.reject(this.#failure)
    if (this.#closed) {
      return Promise.reject(new SessionRuntimeError('operation', 'Operation is complete'))
    }
    return new Promise<SessionMessage>((resolve, reject) => {
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

  push(message: SessionMessage, laneId?: number, laneEpoch?: number): Promise<'delivered' | 'discarded'> {
    const pushed = this.#pushTail.then(() => this.#push(message, laneId, laneEpoch))
    this.#pushTail = pushed.then(() => undefined, () => undefined)
    return pushed
  }

  async #push(
    message: SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ): Promise<'delivered' | 'discarded'> {
    if (this.#closed || this.#failure !== undefined) {
      await this.#authority.acceptLate(message)
      return 'discarded'
    }
    const reservation = await this.#authority.reserve(message)
    if (reservation.disposition === 'drop') return 'discarded'
    if (this.#closed || this.#failure !== undefined) {
      reservation.rollback()
      await this.#authority.acceptLate(message)
      return 'discarded'
    }
    try {
      this.#onAuthenticatedMessage(message, laneId, laneEpoch)
      const reader = this.#readers.shift()
      if (reader === undefined) {
        if (this.#messages.length >= SESSION_OPERATION_RESPONSE_QUEUE) {
          throw new SessionRuntimeError('session', 'Operation response queue is full')
        }
        this.#admission.charge(message)
        this.#messages.push(message)
      } else {
        reader.resolve(message)
      }
      reservation.accept()
      if (reservation.final) this.#finish(false)
      return 'delivered'
    } catch (error) {
      reservation.rollback()
      throw error
    }
  }

  /** Physical input exhaustion preserves authenticated responses already owned by the consumer. */
  endInput(reason: unknown): Promise<void> {
    if (!this.#closed) {
      this.#closed = true
      this.#failure = reason
      this.#authority.retire('local-cancel')
      for (const reader of this.#readers.splice(0)) reader.reject(reason)
      this.#settle(false, 'local_cancel')
    }
    return new Promise(resolve => this.onSettled(resolve))
  }

  fail(reason: unknown): void {
    if (this.#failure !== undefined && this.#consumerSettled) return
    const settlement = isSessionFailure(reason) ? 'session_terminal' : 'local_cancel'
    if (settlement === 'session_terminal') this.#authority.close()
    else this.#authority.retire('local-cancel')
    this.#rejectConsumer(reason, settlement)
  }

  cancel(cause: unknown, protocolReason?: OperationCancelReason): void {
    if (this.#failure !== undefined && this.#consumerSettled) return
    if (protocolReason !== undefined) this.#cancellationReason = cancellationTraceReason(protocolReason)
    this.#authority.retire('local-cancel')
    this.#rejectConsumer(cause, 'local_cancel')
  }

  #rejectConsumer(reason: unknown, settlement: ProtocolOperationSettlement): void {
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
      reader.reject(new SessionRuntimeError('operation', 'Operation is complete'))
    }
    this.#settle(
      acceptLateNonterminal,
      acceptLateNonterminal ? 'local_cancel' : 'remote_final',
    )
  }

  #settle(
    acceptLateNonterminal: boolean,
    settlement: ProtocolOperationSettlement,
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

type SessionControlKind = typeof MESSAGE_KIND.peerPathControl
type SessionControlListener = (body: Uint8Array<ArrayBuffer>) => void

export class OperationRouter {
  readonly #operations = new Map<bigint, OperationQueue>()
  readonly #draining = new Set<OperationQueue>()
  readonly #onTerminal: (reason: unknown) => void
  readonly #tombstones = new Map<bigint, OperationTombstone>()
  readonly #retiredPeers = new RetiredPeerContinuations()
  readonly #compactedResponseTraced = new Set<MessageKind>()
  readonly #admission = new SessionQueueAdmission()
  readonly #diagnostics: ProtocolTraceContext | undefined
  readonly #sessionControls: ReadonlyMap<MessageKind, Set<SessionControlListener>> = new Map([
    [MESSAGE_KIND.peerPathControl, new Set<SessionControlListener>()],
  ])
  readonly #authenticatedResponses = new WeakMap<SessionMessage,
    ReceivedProtocolError | Readonly<{ correlation: ReceivedProtocolError['correlation'] }>>()
  readonly #capacityWaiters = new Set<() => void>()
  #nextOperationId = 1n
  #lastIssuedId = 0n
  #terminal: unknown

  constructor(
    onTerminal: (reason: unknown) => void,
    diagnostics?: ProtocolTraceContext,
  ) {
    this.#onTerminal = onTerminal
    this.#diagnostics = diagnostics
  }

  async admit(
    requestKind: MessageKind,
    canonicalRequestBody: Uint8Array,
    signal?: AbortSignal,
  ): Promise<OperationQueue> {
    signal?.throwIfAborted()
    requireRequestKind(requestKind)
    const ownedBody = canonicalRequestBody.slice()
    let waiting = false
    try {
      for (;;) {
        signal?.throwIfAborted()
        if (this.#terminal !== undefined) {
          throw new SessionRuntimeError('session', 'Protocol session is terminal')
        }
        if (this.#hasOperationCapacity()) {
          const ownedId = this.#issueOperationId()
          const operation = this.#create(ownedId, requestKind, ownedBody)
          if (waiting) this.#traceAdmission(ownedId, requestKind, 'admission_ready')
          return operation
        }
        if (!waiting) {
          waiting = true
          this.#traceAdmission(undefined, requestKind, 'active')
        }
        // Wait before allocating a monotonic ID or entering a lane writer so wire
        // IDs stay strictly ordered by admission and abandoned waits skip no IDs.
        await this.#waitForCapacity(signal)
      }
    } catch (error) {
      if (waiting) this.#traceAdmission(undefined, requestKind, 'admission_abandoned')
      throw error
    }
  }

  create(requestKind: MessageKind, canonicalRequestBody: Uint8Array): OperationQueue {
    requireRequestKind(requestKind)
    if (this.#terminal !== undefined) {
      throw new SessionRuntimeError('session', 'Protocol session is terminal')
    }
    if (this.#operations.size >= MAXIMUM_ACTIVE_OPERATIONS) {
      throw new SessionRuntimeError('session', 'Active operation budget is exhausted')
    }
    return this.#create(this.#issueOperationId(), requestKind, canonicalRequestBody)
  }

  #issueOperationId(): bigint {
    if (this.#nextOperationId > MAXIMUM_OPERATION_ID) {
      throw new SessionRuntimeError('session', 'Operation identity space is exhausted')
    }
    const id = this.#nextOperationId
    this.#nextOperationId += 1n
    this.#lastIssuedId = id
    return id
  }

  #create(
    id: bigint,
    requestKind: MessageKind,
    canonicalRequestBody: Uint8Array,
  ): OperationQueue {
    if (this.#terminal !== undefined) {
      throw new SessionRuntimeError('session', 'Protocol session is terminal')
    }
    if (this.#operations.size >= MAXIMUM_ACTIVE_OPERATIONS) {
      throw new SessionRuntimeError('session', 'Active operation budget is exhausted')
    }
    const operation = new OperationQueue(
      id,
      requestKind,
      canonicalRequestBody,
      this.#admission,
      (authority, settlement) => {
        this.#retiredPeers.retire(authority.peerBinding())
        this.#operations.delete(id)
        this.#draining.add(operation)
        this.#tombstones.set(id, new OperationTombstone(
          authority, settlement, operation.requestTrace, operation.cancellationReason,
        ))
        this.#evictOverflowTombstones()
        this.#wakeCapacityWaiters()
        if (settlement !== 'remote_final') {
          this.#emitSettlement(operation, settlement)
        }
      },
      () => this.#draining.delete(operation),
      (message, laneId, laneEpoch) => {
        this.#captureAuthenticatedResponse(operation, message, laneId, laneEpoch)
        if (message.kind === MESSAGE_KIND.operationError) {
          const failure = decodeOperationErrorControl(message.body)
          if (failure.scope === 'peer' && peerFailureScope(failure.code) === 'session-terminal') {
            const reason = new SessionRuntimeError('session', 'Authenticated peer failure ended the session')
            this.terminate(reason)
            this.#onTerminal(reason)
            throw reason
          }
        }
      },
      this.#diagnostics?.trace?.current === undefined ? undefined : snapshotOperationRequest(requestKind, canonicalRequestBody),
    )
    this.#retiredPeers.admit(operation.peerBinding())
    this.#operations.set(id, operation)
    return operation
  }

  owns(operation: SessionOperation): operation is OperationQueue {
    return this.#operations.get(operation.id) === operation
  }

  active(): readonly OperationQueue[] {
    return Object.freeze([...this.#operations.values()])
  }

  protocolFailureFor(message: SessionMessage): ReceivedProtocolError | undefined {
    const response = this.#authenticatedResponses.get(message)
    return response !== undefined && 'content' in response ? response : undefined
  }

  receivedCorrelationFor(message: SessionMessage): ReceivedProtocolError['correlation'] | undefined {
    return this.#authenticatedResponses.get(message)?.correlation
  }

  subscribePeerPathControls(listener: SessionControlListener): () => void {
    return this.#subscribeSessionControl(MESSAGE_KIND.peerPathControl, listener)
  }

  #subscribeSessionControl(kind: SessionControlKind, listener: SessionControlListener): () => void {
    const listeners = this.#sessionControls.get(kind)!
    listeners.add(listener)
    return () => listeners.delete(listener)
  }

  async route(
    message: SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ): Promise<void> {
    if (this.#terminal !== undefined) return
    if (message.kind === MESSAGE_KIND.sessionTerminal) {
      const reason = new SessionRuntimeError('session', 'Sender ended the protocol session')
      this.terminate(reason)
      this.#onTerminal(reason)
      return
    }
    const listeners = this.#sessionControls.get(message.kind)
    if (listeners !== undefined) {
      for (const listener of listeners) listener(message.body.slice())
      return
    }
    const operationId = message.operationId
    if (operationId === undefined) {
      throw new SessionRuntimeError('session', 'Inbound operation message has no identity')
    }
    const operation = this.#operations.get(operationId)
    if (operation !== undefined) {
      const disposition = await operation.push(message, laneId, laneEpoch)
      if (disposition === 'discarded') {
        this.#tombstones.get(operationId)?.traceResponse(message, this.#diagnostics, laneId, laneEpoch)
        return
      }
      if (message.kind !== MESSAGE_KIND.blockFragment) {
        this.#emitTrace(() => Object.freeze({
          eventName: 'protocol_operation',
          transition: 'response_received',
          requestKind: protocolMessageKindV1(operation.requestKind),
          responseKind: protocolMessageKindV1(message.kind),
          correlation: this.#operationCorrelation(operation, laneId, laneEpoch),
        }))
      }
      const protocolFailure = this.protocolFailureFor(message)
      if (protocolFailure !== undefined) {
        this.#emitTrace(() => Object.freeze({
          eventName: 'protocol_operation',
          transition: 'authenticated_failure',
          requestKind: protocolFailure.requestKind,
          protocolError: protocolFailure.content,
          correlation: protocolFailure.correlation,
        }))
      }
      if (operation.settlement === 'remote_final') {
        this.#emitSettlement(operation, 'remote_final', laneId, laneEpoch)
      }
      return
    }
    await this.#routeRetired(operationId, message, laneId, laneEpoch)
  }

  async #routeRetired(id: bigint, message: SessionMessage, laneId?: number, laneEpoch?: number): Promise<void> {
    const tombstone = this.#tombstones.get(id)
    if (tombstone === undefined) {
      if (this.#retiredPeers.drops(message)) return
      if (id < 1n || id > this.#lastIssuedId) {
        throw new SessionRuntimeError('session', 'Inbound message uses an unknown operation ID')
      }
      const requestKind = inferredRetiredRequestKind(message)
      if (requestKind === undefined) {
        throw new SessionRuntimeError('session', 'Inbound message uses an unknown operation ID')
      }
      // Active operations and recent tombstones were checked first. Earlier issued
      // IDs are retired operations whose tombstone aged out of the FIFO cache,
      // never authority for new work.
      const diagnostics = this.#diagnostics
      // One observation per request kind bounds a stalled fragment burst.
      if (diagnostics?.trace?.current !== undefined && !this.#compactedResponseTraced.has(requestKind)) {
        this.#compactedResponseTraced.add(requestKind)
        this.#emitTrace(() => ({
          eventName: 'protocol_operation', transition: 'retired_response_discarded',
          requestKind: protocolMessageKindV1(requestKind), responseKind: protocolMessageKindV1(message.kind),
          correlation: {
            protocolSessionId: diagnostics.protocolSessionIdentity,
            protocolOperationId: createProtocolOperationIdentity(id),
            ...(laneId === undefined || laneEpoch === undefined ? {} : { lane: { id: laneId, epoch: laneEpoch } }),
          },
        }))
      }
      return
    }
    await tombstone.accept(message)
    tombstone.traceResponse(message, this.#diagnostics, laneId, laneEpoch)
  }

  terminate(reason: unknown): void {
    if (this.#terminal !== undefined) return
    this.#terminal = reason
    this.#wakeCapacityWaiters()
    for (const operation of [...this.#operations.values()]) operation.fail(reason)
    for (const operation of [...this.#draining]) operation.fail(reason)
    for (const tombstone of this.#tombstones.values()) tombstone.close()
    this.#operations.clear()
    this.#draining.clear()
    this.#tombstones.clear()
    this.#retiredPeers.clear()
  }

  #captureAuthenticatedResponse(
    operation: OperationQueue,
    message: SessionMessage,
    laneId?: number,
    laneEpoch?: number,
  ): void {
    if (message.kind !== MESSAGE_KIND.operationError && message.kind !== MESSAGE_KIND.openResults) return
    const diagnostics = this.#diagnostics
    if (diagnostics === undefined) return
    const peerBinding = operation.peerBinding()
    const correlation: ReceivedProtocolError['correlation'] = Object.freeze({
      protocolSessionId: diagnostics.protocolSessionIdentity,
      protocolOperationId: createProtocolOperationIdentity(operation.id),
      ...(peerBinding === undefined
        ? {}
        : {
            peerPathId: createPeerPathIdentityValue(peerBinding.peerPathId),
            peerAttemptId: createPeerAttemptIdentity(peerBinding.attemptId),
          }),
      ...(laneId === undefined || laneEpoch === undefined
        ? {}
        : { lane: Object.freeze({ id: laneId, epoch: laneEpoch }) }),
    })
    if (message.kind === MESSAGE_KIND.openResults) {
      // The content layer decodes per-file failures after final delivery removes
      // the operation binding. Preserve this message's receive lane, not its request route.
      this.#authenticatedResponses.set(message, Object.freeze({ correlation }))
      return
    }
    const decoded = decodeOperationErrorControl(message.body)
    this.#authenticatedResponses.set(message, createReceivedProtocolError({
      requestKind: protocolMessageKindV1(operation.requestKind), correlation, content: {
        scope: decoded.scope, code: decoded.code, retryable: decoded.retryable, ...(decoded.retryAfterMilliseconds === undefined
          ? {}
          : { retryAfterMilliseconds: decoded.retryAfterMilliseconds })
      }
    }))
  }

  #emitSettlement(
    operation: OperationQueue,
    settlement: ProtocolOperationSettlement,
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
    operation: OperationQueue,
    laneId: number | undefined,
    laneEpoch: number | undefined,
  ): ProtocolCorrelation {
    const diagnostics = this.#diagnostics
    if (diagnostics === undefined) {
      throw new SessionRuntimeError('session', 'Protocol trace correlation is unavailable')
    }
    const peerBinding = operation.peerBinding()
    return Object.freeze({
      protocolSessionId: diagnostics.protocolSessionIdentity,
      protocolOperationId: createProtocolOperationIdentity(operation.id),
      ...(peerBinding === undefined
        ? {}
        : {
            peerPathId: createPeerPathIdentityValue(peerBinding.peerPathId),
            peerAttemptId: createPeerAttemptIdentity(peerBinding.attemptId),
          }),
      ...(laneId === undefined || laneEpoch === undefined
        ? {}
        : { lane: Object.freeze({ id: laneId, epoch: laneEpoch }) }),
    })
  }

  #emitTrace(createEvent: () => ProtocolTraceEvent): void {
    try {
      const observer = this.#diagnostics?.trace?.current
      if (observer === undefined) return
      observer(createEvent())
    } catch {
      // Trace failure cannot alter authenticated routing or queue settlement.
    }
  }

  #traceAdmission(
    id: bigint | undefined,
    requestKind: MessageKind,
    stage: 'active' | 'admission_ready' | 'admission_abandoned',
  ): void {
    const diagnostics = this.#diagnostics
    if (diagnostics === undefined) return
    this.#emitTrace(() => ({
      eventName: 'protocol_operation',
      requestKind: protocolMessageKindV1(requestKind),
      correlation: {
        protocolSessionId: diagnostics.protocolSessionIdentity,
        ...(id === undefined ? {} : { protocolOperationId: createProtocolOperationIdentity(id) }),
      },
      ...(stage === 'active'
        ? {
            transition: 'admission_waiting' as const, capacity: stage,
            activeOperations: this.#operations.size,
            trackedOperations: this.#operations.size + this.#tombstones.size,
          }
        : { transition: stage }),
    }))
  }

  #hasOperationCapacity(): boolean {
    return this.#operations.size < MAXIMUM_ACTIVE_OPERATIONS
  }

  #waitForCapacity(signal?: AbortSignal): Promise<void> {
    // A synchronous trace observer may cancel, close the session, or settle an
    // existing operation. Recheck before subscribing so its wakeup is not lost.
    signal?.throwIfAborted()
    if (this.#terminal !== undefined || this.#hasOperationCapacity()) {
      return Promise.resolve()
    }
    return new Promise<void>((resolve, reject) => {
      const cleanup = () => {
        this.#capacityWaiters.delete(wake)
        signal?.removeEventListener('abort', abort)
      }
      const wake = () => {
        cleanup()
        resolve()
      }
      const abort = () => {
        cleanup()
        reject(signal?.reason)
      }
      this.#capacityWaiters.add(wake)
      signal?.addEventListener('abort', abort, { once: true })
    })
  }

  #wakeCapacityWaiters(): void {
    for (const wake of [...this.#capacityWaiters]) wake()
  }

  #evictOverflowTombstones(): void {
    while (this.#tombstones.size > MAXIMUM_RECENT_TOMBSTONES) {
      const oldest = this.#tombstones.entries().next().value
      if (oldest === undefined) break
      oldest[1].close()
      this.#tombstones.delete(oldest[0])
    }
  }
}

class SessionQueueAdmission {
  #control = 0
  #data = 0
  #plaintextBytes = 0

  charge(message: SessionMessage): void {
    const nextControl = this.#control + (message.data ? 0 : 1)
    const nextData = this.#data + (message.data ? 1 : 0)
    const nextBytes = this.#plaintextBytes + message.plaintext.byteLength
    if (
      nextControl > SESSION_CONTROL_BACKLOG ||
      nextData > SESSION_DATA_BACKLOG ||
      nextBytes > SESSION_PLAINTEXT_BACKLOG_BYTES
    ) {
      throw new SessionRuntimeError('session', 'Protocol session response backlog is full')
    }
    this.#control = nextControl
    this.#data = nextData
    this.#plaintextBytes = nextBytes
  }

  release(message: SessionMessage): void {
    if (message.data) this.#data -= 1
    else this.#control -= 1
    this.#plaintextBytes -= message.plaintext.byteLength
    if (this.#control < 0 || this.#data < 0 || this.#plaintextBytes < 0) {
      throw new SessionRuntimeError('session', 'Protocol session response admission underflowed')
    }
  }
}

function isSessionFailure(reason: unknown): boolean {
  return reason instanceof SessionRuntimeError && reason.scope === 'session'
}
