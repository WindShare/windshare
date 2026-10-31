import { cancellationTraceReason } from './operation-diagnostics'
import { OperationCancellationError } from './runtime-types'
import { decodePeerPathControl } from '../protocol/peer-path-control'
import { decodeTurnControl } from '../protocol/turn-control'
import type { ShareDescriptor } from '../catalog/records'
import type { FrameChannel } from '../contracts/channel'
import type { FailureCorrelation } from '../diagnostics/incident/failure-correlation'
import type { ReceivedProtocolError } from '../diagnostics/incident/protocol-error'
import {
  decodeLaneGrant,
  encodeLaneAttachRequest,
  encodeLaneHello,
  type LaneGrant,
  LANE_ACCEPT_BYTES,
  LANE_REJECT_BYTES,
  verifyLaneAccept,
  verifyLaneReject,
} from './lane-codec'
import {
  deadlineSignal,
  laneAdmissionIdentity,
  readLaneAdmission,
  LaneAdmissionRejectedError,
  LaneAdmissionTransportError,
  LaneInstallationError,
  type LaneAdmissionResult,
  type LaneAdoptionOptions,
  type LaneGrantRequestOptions,
} from './lane-admission'
import { SessionLane, type OutboundSend } from './lane-runtime'
import {
  encodeBody,
  encodeMessage,
  type MessageKind,
  type SessionMessage,
  MESSAGE_KIND,
} from './message'
import {
  protocolMessageKindV1,
  type ProtocolTraceEvent,
  type ProtocolTraceSource,
} from './diagnostics'
import {
  createPeerAttemptIdentity,
  createPeerPathIdentityValue,
  createProtocolOperationIdentity,
  createProtocolSessionIdentity,
  type ProtocolSessionIdentity,
} from './identities'
import { OperationQueue, OperationRouter } from './operation-router'
import {
  type LaneChange,
  type LaneTermination,
  OPERATION_CANCEL_REASON,
  type OperationCancelReason,
  type OperationCancellation,
  type ReceiverSessionOptions,
  type SessionOperation,
  SessionRuntimeError,
  SessionHandshakeTimeoutError,
} from './runtime-types'
import {
  createReceiverHandshake,
  type ReceiverHandshake,
  type ReceiverHandshakeOptions,
  type SessionKeys,
} from './transcript'

export const SESSION_HANDSHAKE_TIMEOUT_MILLISECONDS = 30_000

export {
  LaneAdmissionRejectedError,
  LaneAdmissionTransportError,
  LaneInstallationError,
} from './lane-admission'
export type {
  LaneAdmissionResult,
  LaneAdoptionOptions,
  LaneGrantRequestOptions,
} from './lane-admission'
export * from './lane-runtime'
export * from './operation-router'
export * from './runtime-types'

export class ReceiverSessionRuntime {
  readonly descriptor: ShareDescriptor
  readonly keys: SessionKeys
  readonly protocolSessionIdentity: ProtocolSessionIdentity
  readonly receiverInstanceId: Uint8Array<ArrayBuffer>
  readonly #router: OperationRouter
  readonly #lanes = new Map<number, SessionLane>()
  readonly #laneEpochs = new Map<number, number>()
  readonly #laneInstallTails = new Map<number, Promise<void>>()
  readonly #operationLanes = new Map<OperationQueue, number>()
  readonly #operationSends = new Map<SessionOperation, OutboundSend>()
  readonly #laneListeners = new Set<(change: LaneChange) => void>()
  readonly #connectivityCleanup: () => void | Promise<void>
  readonly #protocolTrace: ProtocolTraceSource | undefined
  #closed = false
  #closeTask: Promise<void> | undefined

  private constructor(
    options: ReceiverSessionOptions,
    keys: SessionKeys,
    receiverInstanceId: Uint8Array,
    reader: ReadableStreamDefaultReader<Uint8Array>,
  ) {
    this.descriptor = options.descriptor
    this.keys = keys
    this.protocolSessionIdentity = createProtocolSessionIdentity(keys.protocolSessionId)
    this.receiverInstanceId = receiverInstanceId.slice()
    this.#router = new OperationRouter(
      () => {
        this.close().catch(() => undefined)
      },
      undefined,
      {
        protocolSessionIdentity: this.protocolSessionIdentity,
        ...(options.protocolTrace === undefined ? {} : { trace: options.protocolTrace }),
      },
      options.randomBytes,
    )
    this.#connectivityCleanup = options.connectivityCleanup ?? (() => undefined)
    this.#protocolTrace = options.protocolTrace
    this.#attach(options.initialChannel, reader, keys.initialLaneId, keys.initialLaneEpoch)
  }

  static async connect(options: ReceiverSessionOptions): Promise<ReceiverSessionRuntime> {
    const deadline = deadlineSignal(
      options.signal,
      SESSION_HANDSHAKE_TIMEOUT_MILLISECONDS,
      new SessionHandshakeTimeoutError(SESSION_HANDSHAKE_TIMEOUT_MILLISECONDS),
    )
    const handshakeOptions: ReceiverHandshakeOptions = {
      descriptor: options.descriptor,
      readSecret: options.readSecret,
      ...(options.randomBytes === undefined ? {} : { randomBytes: options.randomBytes }),
    }
    let handshake: ReceiverHandshake | undefined
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined
    try {
      deadline.signal.throwIfAborted()
      handshake = await createReceiverHandshake(handshakeOptions)
      reader = options.initialChannel.frames.getReader()
      await options.initialChannel.send(handshake.clientHello, deadline.signal)
      const response = await readLaneAdmission(reader, deadline.signal)
      const keys = await handshake.acceptServerHello(response)
      deadline.signal.throwIfAborted()
      return new ReceiverSessionRuntime(options, keys, handshake.receiverInstanceId, reader)
    } catch (error) {
      handshake?.discard()
      await reader?.cancel(error).catch(() => undefined)
      reader?.releaseLock()
      await options.initialChannel.close().catch(() => undefined)
      throw error
    } finally {
      deadline.close()
    }
  }

  get initialLaneId(): number {
    return this.keys.initialLaneId
  }

  get isClosed(): boolean {
    return this.#closed
  }

  laneIds(): readonly number[] {
    return Object.freeze([...this.#lanes.keys()])
  }

  operationCorrelation(operation: SessionOperation): FailureCorrelation {
    const laneId = this.#operationLanes.get(operation as OperationQueue)
    const laneEpoch = laneId === undefined ? undefined : this.#laneEpochs.get(laneId)
    const peerBinding = operation instanceof OperationQueue
      ? operation.peerBinding()
      : undefined
    return Object.freeze({
      protocolSessionId: this.protocolSessionIdentity,
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

  authenticatedResponseCorrelation(message: SessionMessage): ReceivedProtocolError['correlation'] {
    const correlation = this.#router.receivedCorrelationFor(message)
    if (correlation === undefined) {
      throw new SessionRuntimeError(
        'session',
        'Authenticated response context was not captured during routing',
      )
    }
    return correlation
  }

  authenticatedReceivedProtocolError(message: SessionMessage): ReceivedProtocolError {
    const failure = this.#router.protocolFailureFor(message)
    if (failure === undefined) {
      throw new SessionRuntimeError(
        'session',
        'Authenticated operation failure was not captured during routing',
      )
    }
    return failure
  }

  subscribeLaneChanges(listener: (change: LaneChange) => void): () => void {
    this.#laneListeners.add(listener)
    return () => this.#laneListeners.delete(listener)
  }

  subscribePeerPathControls(listener: (body: Uint8Array<ArrayBuffer>) => void): () => void {
    return this.#router.subscribePeerPathControls(listener)
  }

  async sendPeerPathControl(body: Uint8Array, options: { readonly laneId?: number; readonly signal?: AbortSignal } = {}): Promise<void> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    decodePeerPathControl(body)
    await this.#selectLane(options.laneId).writer.send(encodeMessage(MESSAGE_KIND.peerPathControl, undefined, body), options)
  }

  subscribeTurnControls(listener: (body: Uint8Array<ArrayBuffer>) => void): () => void {
    return this.#router.subscribeTurnControls(listener)
  }

  async sendTurnControl(body: Uint8Array, options: { readonly laneId?: number; readonly signal?: AbortSignal } = {}): Promise<void> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    decodeTurnControl(body)
    await this.#selectLane(options.laneId).writer.send(encodeMessage(MESSAGE_KIND.turnControl, undefined, body), options)
  }

  async requestLaneGrant(
    requestedLaneId = 0,
    options: LaneGrantRequestOptions = {},
  ): Promise<LaneGrant> {
    const operation = await this.beginOperation(
      MESSAGE_KIND.laneAttach,
      encodeLaneAttachRequest(requestedLaneId),
      options,
    )
    this.#emitProtocolTrace(() => Object.freeze({
      eventName: 'lane_transition',
      transition: 'grant_requested',
      correlation: this.operationCorrelation(operation),
    }))
    const message = await operation.next(options.signal)
    if (message.kind === MESSAGE_KIND.operationError) {
      throw new SessionRuntimeError('lane', 'Sender rejected the lane grant request')
    }
    if (message.kind !== MESSAGE_KIND.laneAttach) {
      throw new SessionRuntimeError('session', 'Lane grant operation received an invalid response')
    }
    let grant: LaneGrant
    try {
      grant = decodeLaneGrant(message.body, operation.id)
      if (requestedLaneId !== 0 && grant.laneId !== requestedLaneId) {
        throw new SessionRuntimeError('session', 'Lane grant changed the requested lane identity')
      }
      const previousEpoch = this.#laneEpochs.get(grant.laneId)
      // The sender owns reusable session slots; path replacement must advance
      // their epoch rather than consume a new identity for the session's lifetime.
      if (previousEpoch !== undefined && grant.laneEpoch <= previousEpoch) {
        throw new SessionRuntimeError('session', 'Lane grant did not advance its logical lane epoch')
      }
    } catch (error) {
      await this.close()
      throw new SessionRuntimeError(
        'session',
        'Authenticated lane grant violated its session identity',
        { cause: error },
      )
    }
    this.#emitProtocolTrace(() => Object.freeze({
      eventName: 'lane_transition',
      transition: 'grant_received',
      correlation: Object.freeze({
        protocolSessionId: this.protocolSessionIdentity,
        protocolOperationId: createProtocolOperationIdentity(grant.grantOperationId),
        lane: Object.freeze({ id: grant.laneId, epoch: grant.laneEpoch }),
      }),
    }))
    return grant
  }

  async adoptGrantedLane(
    channel: FrameChannel,
    grant: LaneGrant,
    options: LaneAdoptionOptions = {},
  ): Promise<LaneAdmissionResult> {
    const signal = options.signal
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined
    let cleanupCause: unknown
    let transferred = false
    try {
      this.#requireOpen()
      signal?.throwIfAborted()
      reader = channel.frames.getReader()
      const hello = await encodeLaneHello({
        shareInstance: this.descriptor.shareInstance,
        protocolSessionId: this.keys.protocolSessionId,
        laneId: grant.laneId,
        laneEpoch: grant.laneEpoch,
        grantOperationId: grant.grantOperationId,
        attachNonce: grant.attachNonce,
      }, this.keys.receiverToSenderKey)
      try {
        await channel.send(hello, signal)
      } catch (cause) {
        if (signal?.aborted) throw signal.reason ?? cause
        throw new LaneAdmissionTransportError('LaneHello delivery failed', { cause })
      }
      this.#emitProtocolTrace(() => Object.freeze({
        eventName: 'lane_transition',
        transition: 'hello_sent',
        correlation: this.#laneGrantCorrelation(grant),
      }))
      const response = await readLaneAdmission(reader, signal)
      this.#emitProtocolTrace(() => Object.freeze({
        eventName: 'lane_transition',
        transition: 'admission_response_received',
        correlation: this.#laneGrantCorrelation(grant),
      }))
      if (response.byteLength === LANE_REJECT_BYTES) {
        const rejection = await verifyLaneReject(
          response,
          hello,
          this.descriptor.sender,
        )
        this.#emitProtocolTrace(() => Object.freeze({
          eventName: 'lane_transition',
          transition: 'admission_rejected',
          rejectionCode: rejection.code,
          retryAfterMilliseconds: rejection.retryAfterMilliseconds,
          correlation: this.#laneGrantCorrelation(grant),
        }))
        const error = new LaneAdmissionRejectedError(rejection)
        cleanupCause = error
        return Object.freeze({
          ...laneAdmissionIdentity(grant),
          disposition: 'rejected',
          installation: 'not-attempted',
          rejection,
          error,
        })
      }
      if (response.byteLength !== LANE_ACCEPT_BYTES) {
        throw new SessionRuntimeError('lane', 'Lane admission response has an invalid length')
      }
      await verifyLaneAccept(response, hello, this.descriptor.sender)
      this.#emitProtocolTrace(() => Object.freeze({
        eventName: 'lane_transition',
        transition: 'admission_accepted',
        correlation: this.#laneGrantCorrelation(grant),
      }))
      try {
        await this.#installAcceptedLane(channel, reader, grant.laneId, grant.laneEpoch)
      } catch (cause) {
        const error = new LaneInstallationError({ cause })
        cleanupCause = error
        return Object.freeze({
          ...laneAdmissionIdentity(grant),
          disposition: 'accepted',
          installation: 'failed',
          error,
        })
      }
      transferred = true
      this.#emitProtocolTrace(() => Object.freeze({
        eventName: 'lane_transition',
        transition: 'installed',
        correlation: this.#laneGrantCorrelation(grant),
      }))
      return Object.freeze({
        ...laneAdmissionIdentity(grant),
        disposition: 'accepted',
        installation: 'installed',
      })
    } catch (error) {
      cleanupCause = error
      return Object.freeze({
        disposition: 'unverified',
        installation: 'not-attempted',
        error,
      })
    } finally {
      if (!transferred) {
        await reader?.cancel(cleanupCause).catch(() => undefined)
        try {
          reader?.releaseLock()
        } catch {
          // A failed admission may already have closed the candidate channel.
        }
        await channel.close().catch(() => undefined)
      }
    }
  }

  async beginOperation(
    kind: MessageKind,
    canonicalBody: Uint8Array,
    options: { readonly laneId?: number; readonly signal?: AbortSignal } = {},
  ): Promise<SessionOperation> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    // Admission may wait; the wire request and continuation binding must own
    // the same snapshot even if the caller subsequently reuses its buffer.
    const requestBody = canonicalBody.slice()
    const operation = await this.#router.admit(kind, requestBody, options.signal)
    try {
      const message = encodeMessage(kind, operation.id, requestBody)
      options.signal?.throwIfAborted()
      const lane = this.#selectLane(options.laneId)
      this.#operationLanes.set(operation, lane.id)
      operation.onSettled(() => this.#operationLanes.delete(operation))
      const delivery = lane.writer.enqueue(message)
      this.#operationSends.set(operation, delivery)
      const cancellationSignal = options.signal
      if (cancellationSignal !== undefined) {
        const cancel = () => {
          const cause = abortCause(cancellationSignal)
          this.cancelOperation(operation, {
            protocolReason: cancellationProtocolReason(cause),
            cause,
            laneId: lane.id,
          }).catch(() => undefined)
        }
        cancellationSignal.addEventListener('abort', cancel, { once: true })
        operation.onSettled(() => cancellationSignal.removeEventListener('abort', cancel))
        if (cancellationSignal.aborted) cancel()
      }
      await delivery.completion
      options.signal?.throwIfAborted()
      this.#emitProtocolTrace(() => Object.freeze({
        eventName: 'protocol_operation',
        transition: 'request_sent',
        requestKind: protocolMessageKindV1(kind),
        correlation: this.operationCorrelation(operation),
      }))
    } catch (error) {
      this.#emitProtocolTrace(() => Object.freeze({
        eventName: 'protocol_operation',
        transition: 'request_send_failed',
        requestKind: protocolMessageKindV1(kind),
        correlation: this.operationCorrelation(operation),
      }))
      operation.cancel(error)
      throw error
    } finally {
      this.#operationSends.delete(operation)
    }
    return operation
  }

  async sendOperationMessage(
    operation: SessionOperation,
    kind: MessageKind,
    canonicalBody: Uint8Array,
    options: { readonly laneId?: number; readonly signal?: AbortSignal } = {},
  ): Promise<void> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    if (!this.#router.owns(operation)) {
      throw new SessionRuntimeError('operation', 'Operation is not active in this session')
    }
    if (!isReceiverFollowupAllowed(operation.requestKind, kind)) {
      throw new SessionRuntimeError('operation', 'Message kind is not valid for this operation')
    }
    const lane = this.#selectLane(options.laneId)
    await lane.writer.send(encodeMessage(kind, operation.id, canonicalBody), options)
  }

  async cancelOperation(
    operation: SessionOperation,
    cancellation: OperationCancellation,
  ): Promise<void> {
    const delivery = this.#operationSends.get(operation)?.cancel(cancellation.cause)
    if (!this.#router.owns(operation)) {
      // A remote final may already have installed its routing tombstone while
      // authenticated responses remain buffered for the consumer. Cancellation
      // still owns releasing that session-wide queue admission.
      operation.cancel(cancellation.cause, cancellation.protocolReason)
      return
    }
    // Local ownership ends before remote I/O. Otherwise a disappeared or
    // backpressured lane can keep an abandoned operation routable forever.
    this.#emitProtocolTrace(() => Object.freeze({
      eventName: 'protocol_operation',
      transition: 'cancelled',
      cancellationReason: cancellationTraceReason(cancellation.protocolReason),
      ...(cancellation.blockWait === undefined ? {} : { blockWait: cancellation.blockWait }),
      ...(operation.requestTrace === undefined ? {} : { request: operation.requestTrace }),
      requestKind: protocolMessageKindV1(operation.requestKind),
      correlation: this.operationCorrelation(operation),
    }))
    for (const lane of this.#lanes.values()) {
      lane.writer.cancelPendingMessages(operation.id, cancellation.cause)
    }
    operation.cancel(cancellation.cause, cancellation.protocolReason)
    // A withdrawn request has no peer-side operation to cancel.
    if (delivery === 'withdrawn') return
    const lane = (cancellation.laneId === undefined
      ? undefined
      : this.#lanes.get(cancellation.laneId)) ??
      this.#lanes.get(this.initialLaneId) ??
      this.#lanes.values().next().value as SessionLane | undefined
    if (lane === undefined) return
    // Local cancellation must not wait behind remote I/O. The writer owns this
    // notification through delivery or lane retirement, including its deadline.
    lane.writer.send(encodeMessage(
      MESSAGE_KIND.cancel,
      operation.id,
      encodeBody([cancellation.protocolReason]),
    )).catch(() => undefined)
  }

  async retireLane(laneId: number, laneEpoch: number): Promise<void> {
    const lane = this.#lanes.get(laneId)
    // Delayed cleanup belongs to its admitted generation, never a reused slot.
    if (lane?.epoch !== laneEpoch) return
    await lane.close('retired')
  }

  async stop(): Promise<void> {
    if (this.#closed) {
      await this.close()
      return
    }
    const cause = new DOMException('Protocol session stopped', 'AbortError')
    for (const operation of this.#router.active()) {
      this.cancelOperation(operation, {
        protocolReason: OPERATION_CANCEL_REASON.user,
        cause,
      }).catch(() => undefined)
    }
    await this.close()
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #close(): Promise<void> {
    this.#closed = true
    this.#router.terminate(new SessionRuntimeError('session', 'Receiver session closed'))
    await Promise.allSettled([...this.#lanes.values()].map((lane) => lane.close('session_closed')))
    this.#lanes.clear()
    this.#laneEpochs.clear()
    this.#laneInstallTails.clear()
    this.#operationLanes.clear()
    this.#laneListeners.clear()
    try {
      await this.#connectivityCleanup()
    } finally {
      this.keys.receiverToSenderKey.fill(0)
      this.keys.senderToReceiverKey.fill(0)
    }
  }

  #attach(
    channel: FrameChannel,
    reader: ReadableStreamDefaultReader<Uint8Array>,
    laneId: number,
    laneEpoch: number,
  ): void {
    const lane = new SessionLane({
      channel,
      reader,
      keys: this.keys,
      descriptor: this.descriptor,
      laneId,
      laneEpoch,
      router: this.#router,
      ...(this.#protocolTrace === undefined ? {} : { protocolTrace: this.#protocolTrace }),
      onClosed: (closed, termination) => {
        // Fatal authenticated failures become non-recoverable before observers
        // see the final lane detach; physical zero-lane loss remains recoverable.
        const fatal = termination.kind === 'authenticated_failure'
        if (fatal) this.#closed = true
        this.#detach(closed, termination.kind === 'closed' ? termination : Object.freeze({
          kind: termination.kind,
          failure: new SessionRuntimeError(
            fatal ? 'session' : 'lane',
            fatal ? 'Authenticated session lane failed' : 'Physical session lane failed',
            { cause: termination.failure },
          ),
        }))
        if (fatal) this.close().catch(() => undefined)
      },
    })
    this.#lanes.set(laneId, lane)
    this.#laneEpochs.set(laneId, laneEpoch)
    this.#emitProtocolTrace(() => Object.freeze({
      eventName: 'lane_transition',
      transition: 'attached',
      correlation: Object.freeze({
        protocolSessionId: this.protocolSessionIdentity,
        lane: Object.freeze({ id: laneId, epoch: laneEpoch }),
      }),
    }))
    this.#emitLaneChange({ type: 'attached', laneId, laneEpoch })
  }

  #selectLane(id?: number): SessionLane {
    if (id === undefined) {
      const initial = this.#lanes.get(this.initialLaneId)
      if (initial !== undefined) return initial
      const available = this.#lanes.values().next().value as SessionLane | undefined
      if (available !== undefined) return available
      throw new SessionRuntimeError('lane', 'No session lane is available')
    }
    const lane = this.#lanes.get(id)
    if (lane === undefined) throw new SessionRuntimeError('lane', 'Requested lane is unavailable')
    return lane
  }

  async #installAcceptedLane(
    channel: FrameChannel,
    reader: ReadableStreamDefaultReader<Uint8Array>,
    laneId: number,
    laneEpoch: number,
  ): Promise<void> {
    const previous = this.#laneInstallTails.get(laneId) ?? Promise.resolve()
    const install = previous.catch(() => undefined).then(async () => {
      this.#requireOpen()
      const acceptedEpoch = this.#laneEpochs.get(laneId)
      if (acceptedEpoch !== undefined && laneEpoch <= acceptedEpoch) {
        throw new SessionRuntimeError('lane', 'Lane reattachment did not advance its epoch')
      }
      const current = this.#lanes.get(laneId)
      if (current !== undefined) {
        await current.close('replaced')
      }
      this.#requireOpen()
      const latestEpoch = this.#laneEpochs.get(laneId)
      if (latestEpoch !== undefined && laneEpoch <= latestEpoch) {
        throw new SessionRuntimeError('lane', 'Concurrent lane admission lost its epoch race')
      }
      this.#attach(channel, reader, laneId, laneEpoch)
    })
    this.#laneInstallTails.set(laneId, install)
    try {
      await install
    } finally {
      if (this.#laneInstallTails.get(laneId) === install) this.#laneInstallTails.delete(laneId)
    }
  }

  #detach(
    lane: SessionLane,
    termination: LaneTermination,
  ): void {
    if (this.#lanes.get(lane.id) !== lane) return
    this.#lanes.delete(lane.id)
    const failure = termination.kind === 'closed' ? undefined : termination.failure
    const reason = failure ?? new SessionRuntimeError('lane', 'Operation lane became unavailable')
    for (const [operation, laneId] of this.#operationLanes) {
      if (laneId === lane.id) operation.fail(reason)
    }
    this.#emitProtocolTrace(() => Object.freeze({
      eventName: 'lane_transition',
      transition: 'detached',
      termination,
      correlation: Object.freeze({
        protocolSessionId: this.protocolSessionIdentity,
        lane: Object.freeze({ id: lane.id, epoch: lane.epoch }),
      }),
    }))
    this.#emitLaneChange({
      type: 'detached',
      laneId: lane.id,
      laneEpoch: lane.epoch,
      ...(failure === undefined ? {} : { failure }),
    })
  }

  #emitLaneChange(change: LaneChange): void {
    for (const listener of this.#laneListeners) {
      try {
        listener(change)
      } catch {
        // Observers cannot own or destabilize authenticated lane lifecycle.
      }
    }
  }

  #laneGrantCorrelation(grant: LaneGrant): FailureCorrelation {
    return Object.freeze({
      protocolSessionId: this.protocolSessionIdentity,
      protocolOperationId: createProtocolOperationIdentity(grant.grantOperationId),
      lane: Object.freeze({ id: grant.laneId, epoch: grant.laneEpoch }),
    })
  }

  #emitProtocolTrace(createEvent: () => ProtocolTraceEvent): void {
    try {
      const observer = this.#protocolTrace?.current
      if (observer === undefined) return
      observer(createEvent())
    } catch {
      // Diagnostic observation cannot acquire protocol or lane authority.
    }
  }

  #requireOpen(): void {
    if (this.#closed) throw new SessionRuntimeError('session', 'Receiver session is closed')
  }
}

function isReceiverFollowupAllowed(request: MessageKind, followup: MessageKind): boolean {
  return request === MESSAGE_KIND.peerOffer && followup === MESSAGE_KIND.peerCandidate
}


function abortCause(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException('Operation aborted', 'AbortError')
}

function cancellationProtocolReason(cause: unknown): OperationCancelReason {
  if (cause instanceof OperationCancellationError) return cause.protocolReason
  return cause instanceof DOMException && cause.name === 'TimeoutError'
    ? OPERATION_CANCEL_REASON.timeout
    : OPERATION_CANCEL_REASON.user
}
