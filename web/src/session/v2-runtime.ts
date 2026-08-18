import type { V2ShareDescriptor } from '../catalog/v2-records'
import type { FrameChannel } from '../contracts/channel'
import { encodeBase64Url } from '../crypto/bytes'
import {
  decodeV2LaneGrant,
  encodeV2LaneAttachRequest,
  encodeV2LaneHello,
  type V2LaneGrant,
  type V2LaneRejection,
  V2_LANE_ACCEPT_BYTES,
  V2_LANE_REJECT_BYTES,
  verifyV2LaneAccept,
  verifyV2LaneReject,
} from './v2-lane-codec'
import { V2SessionLane } from './v2-lane-runtime'
import {
  encodeV2Body,
  encodeV2Message,
  type V2MessageKind,
  V2_MESSAGE_KIND,
} from './v2-message'
import { V2OperationRouter, type V2OperationQueue } from './v2-operation-router'
import {
  type V2LaneChange,
  V2_OPERATION_CANCEL_REASON,
  type V2OperationCancelReason,
  type V2OperationCancellation,
  type V2ReceiverSessionOptions,
  type V2SessionOperation,
  V2SessionRuntimeError,
} from './v2-runtime-types'
import {
  createV2ReceiverHandshake,
  type V2ReceiverHandshake,
  type V2ReceiverHandshakeOptions,
  type V2SessionKeys,
} from './v2-transcript'

export const V2_SESSION_HANDSHAKE_TIMEOUT_MILLISECONDS = 30_000

export * from './v2-lane-runtime'
export * from './v2-operation-router'
export * from './v2-runtime-types'

export class V2LaneAdmissionRejectedError extends V2SessionRuntimeError {
  readonly rejection: V2LaneRejection

  constructor(rejection: V2LaneRejection) {
    super('lane', `Sender rejected lane admission (code ${rejection.code})`)
    this.name = 'V2LaneAdmissionRejectedError'
    this.rejection = rejection
  }
}

export class V2LaneAdmissionTransportError extends V2SessionRuntimeError {
  constructor(message: string, options?: ErrorOptions) {
    super('lane', message, options)
    this.name = 'V2LaneAdmissionTransportError'
  }
}

export class V2LaneInstallationError extends V2SessionRuntimeError {
  constructor(options?: ErrorOptions) {
    super('lane', 'Accepted lane could not be installed', options)
    this.name = 'V2LaneInstallationError'
  }
}

interface V2LaneAdmissionIdentity {
  readonly grantOperationId: string
  readonly laneId: number
  readonly laneEpoch: number
}

/**
 * Authentication and local installation are separate settlement facts. Keeping
 * them in one closed result prevents a later lifecycle signal from erasing a
 * sender-authenticated disposition when publication fails locally.
 */
export type V2LaneAdmissionResult =
  | {
      readonly disposition: 'unverified'
      readonly installation: 'not-attempted'
      readonly error: unknown
    }
  | (V2LaneAdmissionIdentity & {
      readonly disposition: 'rejected'
      readonly installation: 'not-attempted'
      readonly rejection: V2LaneRejection
      readonly error: V2LaneAdmissionRejectedError
    })
  | (V2LaneAdmissionIdentity & {
      readonly disposition: 'accepted'
      readonly installation: 'failed'
      readonly error: V2LaneInstallationError
    })
  | (V2LaneAdmissionIdentity & {
      readonly disposition: 'accepted'
      readonly installation: 'installed'
    })

export type V2LaneAdmissionMilestone =
  | {
      readonly type: 'grant-requested' | 'grant-received' | 'lane-hello-sent'
      readonly grantOperationId: string
      readonly laneId: number
      readonly laneEpoch: number
    }
  | {
      readonly type: 'admission-response-accepted'
      readonly grantOperationId: string
      readonly laneId: number
      readonly laneEpoch: number
    }
  | {
      readonly type: 'admission-response-rejected'
      readonly grantOperationId: string
      readonly laneId: number
      readonly laneEpoch: number
      readonly rejection: V2LaneRejection
    }
  | {
      readonly type: 'lane-adopted'
      readonly grantOperationId: string
      readonly laneId: number
      readonly laneEpoch: number
    }

export type V2LaneAdmissionObserver = (milestone: V2LaneAdmissionMilestone) => void

export interface V2LaneGrantRequestOptions {
  readonly laneId?: number
  readonly signal?: AbortSignal
  readonly observeAdmission?: V2LaneAdmissionObserver
}

export interface V2LaneAdoptionOptions {
  readonly signal?: AbortSignal
  readonly observeAdmission?: V2LaneAdmissionObserver
}

export class V2ReceiverSessionRuntime {
  readonly descriptor: V2ShareDescriptor
  readonly keys: V2SessionKeys
  readonly receiverInstanceId: Uint8Array<ArrayBuffer>
  readonly #router: V2OperationRouter
  readonly #lanes = new Map<number, V2SessionLane>()
  readonly #laneEpochs = new Map<number, number>()
  readonly #laneInstallTails = new Map<number, Promise<void>>()
  readonly #operationLanes = new Map<V2OperationQueue, number>()
  readonly #laneListeners = new Set<(change: V2LaneChange) => void>()
  readonly #randomBytes: (length: number) => Uint8Array
  readonly #connectivityCleanup: () => void | Promise<void>
  #closed = false
  #closeTask: Promise<void> | undefined

  private constructor(
    options: V2ReceiverSessionOptions,
    keys: V2SessionKeys,
    receiverInstanceId: Uint8Array,
    reader: ReadableStreamDefaultReader<Uint8Array>,
  ) {
    this.descriptor = options.descriptor
    this.keys = keys
    this.receiverInstanceId = receiverInstanceId.slice()
    this.#router = new V2OperationRouter(() => {
      this.close().catch(() => undefined)
    })
    this.#randomBytes = options.randomBytes ?? secureRandomBytes
    this.#connectivityCleanup = options.connectivityCleanup ?? (() => undefined)
    this.#attach(options.initialChannel, reader, keys.initialLaneId, keys.initialLaneEpoch)
  }

  static async connect(options: V2ReceiverSessionOptions): Promise<V2ReceiverSessionRuntime> {
    const deadline = deadlineSignal(
      options.signal,
      V2_SESSION_HANDSHAKE_TIMEOUT_MILLISECONDS,
      'Protocol session handshake timed out',
      'session',
    )
    const handshakeOptions: V2ReceiverHandshakeOptions = {
      descriptor: options.descriptor,
      readSecret: options.readSecret,
      ...(options.randomBytes === undefined ? {} : { randomBytes: options.randomBytes }),
    }
    let handshake: V2ReceiverHandshake | undefined
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined
    try {
      deadline.signal.throwIfAborted()
      handshake = await createV2ReceiverHandshake(handshakeOptions)
      reader = options.initialChannel.frames.getReader()
      await options.initialChannel.send(handshake.clientHello, deadline.signal)
      const response = await readLaneAdmission(reader, deadline.signal)
      const keys = await handshake.acceptServerHello(response)
      deadline.signal.throwIfAborted()
      return new V2ReceiverSessionRuntime(options, keys, handshake.receiverInstanceId, reader)
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

  subscribeLaneChanges(listener: (change: V2LaneChange) => void): () => void {
    this.#laneListeners.add(listener)
    return () => this.#laneListeners.delete(listener)
  }

  async requestLaneGrant(
    requestedLaneId = 0,
    options: V2LaneGrantRequestOptions = {},
  ): Promise<V2LaneGrant> {
    const operation = await this.beginOperation(
      V2_MESSAGE_KIND.laneAttach,
      encodeV2LaneAttachRequest(requestedLaneId),
      options,
    )
    emitLaneAdmission(options.observeAdmission, {
      type: 'grant-requested',
      grantOperationId: encodeBase64Url(operation.id),
      laneId: requestedLaneId,
      laneEpoch: 0,
    })
    const message = await operation.next(options.signal)
    if (message.kind === V2_MESSAGE_KIND.operationError) {
      throw new V2SessionRuntimeError('lane', 'Sender rejected the lane grant request')
    }
    if (message.kind !== V2_MESSAGE_KIND.laneAttach) {
      throw new V2SessionRuntimeError('session', 'Lane grant operation received an invalid response')
    }
    let grant: V2LaneGrant
    try {
      grant = decodeV2LaneGrant(message.body, operation.id)
      if (requestedLaneId !== 0 && grant.laneId !== requestedLaneId) {
        throw new V2SessionRuntimeError('session', 'Lane grant changed the requested lane identity')
      }
      if (requestedLaneId === 0 && this.#laneEpochs.has(grant.laneId)) {
        throw new V2SessionRuntimeError('session', 'Automatic lane grant reused a logical lane identity')
      }
    } catch (error) {
      await this.close()
      throw new V2SessionRuntimeError(
        'session',
        'Authenticated lane grant violated its session identity',
        { cause: error },
      )
    }
    emitLaneAdmission(options.observeAdmission, grantMilestone('grant-received', grant))
    return grant
  }

  async adoptGrantedLane(
    channel: FrameChannel,
    grant: V2LaneGrant,
    options: V2LaneAdoptionOptions = {},
  ): Promise<V2LaneAdmissionResult> {
    const signal = options.signal
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined
    let cleanupCause: unknown
    let transferred = false
    try {
      this.#requireOpen()
      signal?.throwIfAborted()
      reader = channel.frames.getReader()
      const hello = await encodeV2LaneHello({
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
        throw new V2LaneAdmissionTransportError('LaneHello delivery failed', { cause })
      }
      emitLaneAdmission(options.observeAdmission, grantMilestone('lane-hello-sent', grant))
      const response = await readLaneAdmission(reader, signal)
      if (response.byteLength === V2_LANE_REJECT_BYTES) {
        const rejection = await verifyV2LaneReject(
          response,
          hello,
          this.descriptor.senderPublicKey,
        )
        emitLaneAdmission(options.observeAdmission, {
          ...grantMilestone('admission-response-rejected', grant),
          rejection,
        })
        const error = new V2LaneAdmissionRejectedError(rejection)
        cleanupCause = error
        return Object.freeze({
          ...laneAdmissionIdentity(grant),
          disposition: 'rejected',
          installation: 'not-attempted',
          rejection,
          error,
        })
      }
      if (response.byteLength !== V2_LANE_ACCEPT_BYTES) {
        throw new V2SessionRuntimeError('lane', 'Lane admission response has an invalid length')
      }
      await verifyV2LaneAccept(response, hello, this.descriptor.senderPublicKey)
      emitLaneAdmission(
        options.observeAdmission,
        grantMilestone('admission-response-accepted', grant),
      )
      try {
        await this.#installAcceptedLane(channel, reader, grant.laneId, grant.laneEpoch)
      } catch (cause) {
        const error = new V2LaneInstallationError({ cause })
        cleanupCause = error
        return Object.freeze({
          ...laneAdmissionIdentity(grant),
          disposition: 'accepted',
          installation: 'failed',
          error,
        })
      }
      transferred = true
      emitLaneAdmission(options.observeAdmission, grantMilestone('lane-adopted', grant))
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
    kind: V2MessageKind,
    canonicalBody: Uint8Array,
    options: { readonly laneId?: number; readonly signal?: AbortSignal } = {},
  ): Promise<V2SessionOperation> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    if (!isReceiverRequestKind(kind)) {
      throw new V2SessionRuntimeError('operation', 'Message kind cannot begin a receiver operation')
    }
    const lane = this.#selectLane(options.laneId)
    const id = nonzeroRandom(this.#randomBytes, 16)
    const message = encodeV2Message(kind, id, canonicalBody)
    const operation = this.#router.create(id, kind, canonicalBody)
    this.#operationLanes.set(operation, lane.id)
    operation.onSettled(() => this.#operationLanes.delete(operation))
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
    }
    try {
      await lane.writer.send(message)
    } catch (error) {
      operation.cancel(error)
      throw error
    }
    return operation
  }

  async sendOperationMessage(
    operation: V2SessionOperation,
    kind: V2MessageKind,
    canonicalBody: Uint8Array,
    options: { readonly laneId?: number; readonly signal?: AbortSignal } = {},
  ): Promise<void> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    if (!this.#router.owns(operation)) {
      throw new V2SessionRuntimeError('operation', 'Operation is not active in this session')
    }
    if (!isReceiverFollowupAllowed(operation.requestKind, kind)) {
      throw new V2SessionRuntimeError('operation', 'Message kind is not valid for this operation')
    }
    const lane = this.#selectLane(options.laneId)
    await lane.writer.send(encodeV2Message(kind, operation.id, canonicalBody))
  }

  async cancelOperation(
    operation: V2SessionOperation,
    cancellation: V2OperationCancellation,
  ): Promise<void> {
    if (!this.#router.owns(operation)) {
      // A remote final may already have installed its routing tombstone while
      // authenticated responses remain buffered for the consumer. Cancellation
      // still owns releasing that session-wide queue admission.
      operation.cancel(cancellation.cause)
      return
    }
    // Local ownership ends before remote I/O. Otherwise a disappeared or
    // backpressured lane can keep an abandoned operation routable forever.
    operation.cancel(cancellation.cause)
    const lane = (cancellation.laneId === undefined
      ? undefined
      : this.#lanes.get(cancellation.laneId)) ??
      this.#lanes.get(this.initialLaneId) ??
      this.#lanes.values().next().value as V2SessionLane | undefined
    if (lane === undefined) return
    await lane.writer.send(encodeV2Message(
      V2_MESSAGE_KIND.cancel,
      operation.id,
      encodeV2Body([cancellation.protocolReason]),
    ))
  }

  async stop(): Promise<void> {
    if (this.#closed) {
      await this.close()
      return
    }
    const cause = new DOMException('Protocol session stopped', 'AbortError')
    for (const operation of this.#router.active()) {
      this.cancelOperation(operation, {
        protocolReason: V2_OPERATION_CANCEL_REASON.user,
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
    this.#router.terminate(new V2SessionRuntimeError('session', 'Receiver session closed'))
    await Promise.allSettled([...this.#lanes.values()].map((lane) => lane.close()))
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
    const lane = new V2SessionLane({
      channel,
      reader,
      keys: this.keys,
      descriptor: this.descriptor,
      laneId,
      laneEpoch,
      router: this.#router,
      onClosed: (closed, failure, fatal) => {
        // Fatal authenticated failures become non-recoverable before observers
        // see the final lane detach; physical zero-lane loss remains recoverable.
        if (fatal) this.#closed = true
        const scopedFailure = fatal
          ? new V2SessionRuntimeError('session', 'Authenticated session lane failed', { cause: failure })
          : new V2SessionRuntimeError('lane', 'Physical session lane failed', { cause: failure })
        this.#detach(closed, scopedFailure)
        if (fatal) this.close().catch(() => undefined)
      },
    })
    this.#lanes.set(laneId, lane)
    this.#laneEpochs.set(laneId, laneEpoch)
    this.#emitLaneChange({ type: 'attached', laneId, laneEpoch })
  }

  #selectLane(id?: number): V2SessionLane {
    if (id === undefined) {
      const initial = this.#lanes.get(this.initialLaneId)
      if (initial !== undefined) return initial
      const available = this.#lanes.values().next().value as V2SessionLane | undefined
      if (available !== undefined) return available
      throw new V2SessionRuntimeError('lane', 'No session lane is available')
    }
    const lane = this.#lanes.get(id)
    if (lane === undefined) throw new V2SessionRuntimeError('lane', 'Requested lane is unavailable')
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
        throw new V2SessionRuntimeError('lane', 'Lane reattachment did not advance its epoch')
      }
      const current = this.#lanes.get(laneId)
      if (current !== undefined) {
        await current.close()
        this.#detach(current)
      }
      this.#requireOpen()
      const latestEpoch = this.#laneEpochs.get(laneId)
      if (latestEpoch !== undefined && laneEpoch <= latestEpoch) {
        throw new V2SessionRuntimeError('lane', 'Concurrent lane admission lost its epoch race')
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

  #detach(lane: V2SessionLane, failure?: unknown): void {
    if (this.#lanes.get(lane.id) !== lane) return
    this.#lanes.delete(lane.id)
    const reason = failure ?? new V2SessionRuntimeError('lane', 'Operation lane became unavailable')
    for (const [operation, laneId] of this.#operationLanes) {
      if (laneId === lane.id) operation.fail(reason)
    }
    this.#emitLaneChange({
      type: 'detached',
      laneId: lane.id,
      laneEpoch: lane.epoch,
      ...(failure === undefined ? {} : { failure }),
    })
  }

  #emitLaneChange(change: V2LaneChange): void {
    for (const listener of this.#laneListeners) {
      try {
        listener(change)
      } catch {
        // Observers cannot own or destabilize authenticated lane lifecycle.
      }
    }
  }

  #requireOpen(): void {
    if (this.#closed) throw new V2SessionRuntimeError('session', 'Receiver session is closed')
  }
}

function isReceiverRequestKind(kind: V2MessageKind): boolean {
  return kind === V2_MESSAGE_KIND.listChildren ||
    kind === V2_MESSAGE_KIND.openRevisions ||
    kind === V2_MESSAGE_KIND.renewLease ||
    kind === V2_MESSAGE_KIND.releaseLease ||
    kind === V2_MESSAGE_KIND.requestBlocks ||
    kind === V2_MESSAGE_KIND.laneAttach ||
    kind === V2_MESSAGE_KIND.peerOffer
}

function isReceiverFollowupAllowed(request: V2MessageKind, followup: V2MessageKind): boolean {
  return request === V2_MESSAGE_KIND.peerOffer && followup === V2_MESSAGE_KIND.peerCandidate
}

async function readLaneAdmission(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  signal?: AbortSignal,
): Promise<Uint8Array<ArrayBuffer>> {
  signal?.throwIfAborted()
  if (signal === undefined) {
    const result = await reader.read()
    if (result.done) {
      throw new V2LaneAdmissionTransportError('Candidate lane closed before admission')
    }
    return result.value.slice()
  }
  return new Promise<Uint8Array<ArrayBuffer>>((resolve, reject) => {
    let settled = false
    const finish = (operation: () => void) => {
      if (settled) return
      settled = true
      signal.removeEventListener('abort', aborted)
      operation()
    }
    const aborted = () => {
      const reason = signal.reason ?? new DOMException('Lane admission aborted', 'AbortError')
      reader.cancel(reason).catch(() => undefined)
      finish(() => reject(reason))
    }
    signal.addEventListener('abort', aborted, { once: true })
    reader.read().then(
      (result) => finish(() => {
        if (result.done) {
          reject(new V2LaneAdmissionTransportError('Candidate lane closed before admission'))
        } else {
          resolve(result.value.slice())
        }
      }),
      (error: unknown) => finish(() => reject(error)),
    )
  })
}

function secureRandomBytes(length: number): Uint8Array<ArrayBuffer> {
  const output = new Uint8Array(length)
  globalThis.crypto.getRandomValues(output)
  return output
}

function nonzeroRandom(
  source: (length: number) => Uint8Array,
  length: number,
): Uint8Array<ArrayBuffer> {
  for (let attempt = 0; attempt < 4; attempt += 1) {
    const value = source(length)
    if (value.byteLength === length && value.some((item) => item !== 0)) return value.slice()
  }
  throw new V2SessionRuntimeError('session', 'Random identity source returned invalid bytes')
}

function abortCause(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException('Operation aborted', 'AbortError')
}

function cancellationProtocolReason(cause: unknown): V2OperationCancelReason {
  return cause instanceof DOMException && cause.name === 'TimeoutError'
    ? V2_OPERATION_CANCEL_REASON.timeout
    : V2_OPERATION_CANCEL_REASON.user
}

function grantMilestone<T extends V2LaneAdmissionMilestone['type']>(
  type: T,
  grant: V2LaneGrant,
): Extract<V2LaneAdmissionMilestone, { readonly type: T }> {
  return Object.freeze({
    type,
    grantOperationId: encodeBase64Url(grant.grantOperationId),
    laneId: grant.laneId,
    laneEpoch: grant.laneEpoch,
  }) as Extract<V2LaneAdmissionMilestone, { readonly type: T }>
}

function laneAdmissionIdentity(grant: V2LaneGrant): V2LaneAdmissionIdentity {
  return Object.freeze({
    grantOperationId: encodeBase64Url(grant.grantOperationId),
    laneId: grant.laneId,
    laneEpoch: grant.laneEpoch,
  })
}

function emitLaneAdmission(
  observer: V2LaneAdmissionObserver | undefined,
  milestone: V2LaneAdmissionMilestone,
): void {
  try {
    observer?.(Object.freeze(milestone))
  } catch {
    // Admission evidence is observational; authenticated state has already advanced.
  }
}

function deadlineSignal(
  parent: AbortSignal | undefined,
  milliseconds: number,
  message: string,
  scope: 'lane' | 'session',
): { readonly signal: AbortSignal; readonly close: () => void } {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent?.reason ?? new DOMException('Operation aborted', 'AbortError'),
  )
  parent?.addEventListener('abort', abort, { once: true })
  if (parent?.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(new V2SessionRuntimeError(scope, message))
  }, milliseconds)
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent?.removeEventListener('abort', abort)
    },
  }
}
