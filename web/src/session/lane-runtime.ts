import { observeProtocolFailure } from './failure-diagnostics'
import { bindFailureEvidence, captureFailureEvidence } from '../diagnostics/failure/evidence'
import type { ShareDescriptor } from '../catalog/records'
import type { FrameChannel } from '../contracts/channel'
import { CborError } from '../protocol/cbor'
import { EnvelopeError, EnvelopeOpener, EnvelopeSealer } from './envelope'
import { protocolMessageKindV1, type ProtocolTraceSource } from './diagnostics'
import { createProtocolOperationIdentity, createProtocolSessionIdentity } from './identities'
import { SessionWriter } from './writer'
export * from './writer'
import {
  decodeMessage,
  MessageError,
  verifySenderControl,
} from './message'
import type { OperationRouter } from './operation-router'
import { SessionRuntimeError, type LaneCloseReason, type LaneTermination } from './runtime-types'
import type { SessionKeys } from './transcript'

export class SessionLane {
  readonly id: number
  readonly epoch: number
  readonly writer: SessionWriter
  readonly #channel: FrameChannel
  readonly #reader: ReadableStreamDefaultReader<Uint8Array>
  readonly #opener: EnvelopeOpener
  readonly #router: OperationRouter
  readonly #descriptor: ShareDescriptor
  readonly #sessionId: Uint8Array<ArrayBuffer>
  readonly #onClosed: (lane: SessionLane, termination: LaneTermination) => void
  readonly #pumpTask: Promise<void>
  readonly #protocolTrace: ProtocolTraceSource | undefined
  #closed = false
  #termination: LaneTermination | undefined
  #closeTask: Promise<void> | undefined

  constructor(options: {
    readonly channel: FrameChannel
    readonly reader: ReadableStreamDefaultReader<Uint8Array>
    readonly keys: SessionKeys
    readonly descriptor: ShareDescriptor
    readonly laneId: number
    readonly laneEpoch: number
    readonly router: OperationRouter
    readonly onClosed: (lane: SessionLane, termination: LaneTermination) => void
    readonly protocolTrace?: ProtocolTraceSource
  }) {
    this.#protocolTrace = options.protocolTrace
    this.id = options.laneId
    this.epoch = options.laneEpoch
    this.#channel = options.channel
    this.#reader = options.reader
    this.#router = options.router
    this.#descriptor = options.descriptor
    this.#sessionId = options.keys.protocolSessionId.slice()
    this.#onClosed = options.onClosed
    this.writer = new SessionWriter(
      options.channel,
      new EnvelopeSealer(options.keys.receiverToSenderKey, {
        shareInstance: options.descriptor.shareInstance,
        protocolSessionId: options.keys.protocolSessionId,
        laneId: options.laneId,
        laneEpoch: options.laneEpoch,
        direction: 0,
      }),
      {
        onFailure: reason => {
          const failure = new SessionRuntimeError('lane', 'Session lane delivery failed', { cause: reason })
          bindFailureEvidence(failure, captureFailureEvidence(reason, { operation: 'session.send' }))
          this.#recordFailure(failure)
          this.#shutdown().catch(() => undefined)
        },
        observe: (message, transition, evidence) => options.protocolTrace?.current?.({
          eventName: 'protocol_operation',
          transition,
          ...(evidence === undefined ? {} : { evidence }),
          requestKind: protocolMessageKindV1(message.kind),
          correlation: Object.freeze({
            protocolSessionId: createProtocolSessionIdentity(this.#sessionId),
            ...(message.operationId === undefined ? {} : {
              protocolOperationId: createProtocolOperationIdentity(message.operationId),
            }),
            lane: Object.freeze({ id: this.id, epoch: this.epoch }),
          }),
        }),
      },
    )
    this.#opener = new EnvelopeOpener(options.keys.senderToReceiverKey, {
      shareInstance: options.descriptor.shareInstance,
      protocolSessionId: options.keys.protocolSessionId,
      laneId: options.laneId,
      laneEpoch: options.laneEpoch,
      direction: 1,
    })
    this.#pumpTask = this.#pump()
  }

  close(reason: LaneCloseReason): Promise<void> {
    this.#termination ??= Object.freeze({ kind: 'closed', reason })
    return this.#shutdown()
  }

  #shutdown(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #close(): Promise<void> {
    if (!this.#closed) {
      this.#closed = true
      this.writer.fail(new SessionRuntimeError('lane', 'Session lane closed'))
      await this.#reader.cancel().catch(error => this.#observeCleanup(error, 'session.reader.cancel'))
    }
    try {
      this.#reader.releaseLock()
    } catch {
      // The reader pump may have observed the cancellation first.
    }
    try { await this.#channel.close() } catch (error) {
      this.#observeCleanup(error, 'session.channel.close')
      throw error
    }
    await this.#pumpTask
  }

  async #readFrame(): Promise<ReadableStreamReadResult<Uint8Array>> {
    try {
      return await this.#reader.read()
    } catch (cause) {
      // Read failure also rejects pending writes. Give them the same physical
      // failure boundary without reclassifying later authentication or parsing.
      const failure = new SessionRuntimeError('lane', 'Session lane receive failed', { cause })
      bindFailureEvidence(failure, captureFailureEvidence(cause, { operation: 'session.read' }))
      throw failure
    }
  }

  #recordFailure(failure: unknown): void {
    const kind = isFatalSessionFailure(failure) ? 'authenticated_failure' : 'physical_failure'
    observeProtocolFailure(this.#protocolTrace, failure, 'session.receive', {
      protocolSessionId: createProtocolSessionIdentity(this.#sessionId),
      lane: Object.freeze({ id: this.id, epoch: this.epoch }),
    }, { closed: this.#closed }, this.#termination !== undefined && kind !== 'authenticated_failure' ? 'consequence' : undefined)
    // Cleanup must not overwrite an observed failure. Authentication still wins
    // when verification of an already-read frame finishes during local closure.
    if (this.#termination === undefined || kind === 'authenticated_failure') {
      this.#termination = Object.freeze({ kind, failure })
    }
  }

  #observeCleanup(error: unknown, operation: string): void {
    observeProtocolFailure(this.#protocolTrace, error, operation, {
      protocolSessionId: createProtocolSessionIdentity(this.#sessionId),
      lane: Object.freeze({ id: this.id, epoch: this.epoch }),
    }, { termination: this.#termination?.kind ?? 'unknown' }, 'consequence')
  }

  async #pump(): Promise<void> {
    try {
      while (!this.#closed) {
        const result = await this.#readFrame()
        if (result.done) break
        const opened = await this.#opener.open(result.value)
        const message = decodeMessage(opened.plaintext)
        const authenticatedBody = await verifySenderControl(
          message,
          {
            shareInstance: this.#descriptor.shareInstance,
            protocolSessionId: this.#sessionId,
            laneId: this.id,
            laneEpoch: this.epoch,
            direction: 1,
            sequence: opened.sequence,
          },
          this.#descriptor.sender,
        )
        await this.#router.route(
          message.data
            ? message
            : Object.freeze({ ...message, body: authenticatedBody }),
          this.id,
          this.epoch,
        )
      }
    } catch (error) {
      this.#recordFailure(error)
      this.writer.fail(error)
    } finally {
      if (this.#termination === undefined) {
        this.#recordFailure(new SessionRuntimeError('lane', 'Session lane receive stream ended unexpectedly'))
      }
      const termination = this.#termination!
      this.#closed = true
      this.writer.fail(termination.kind === 'closed'
        ? new SessionRuntimeError('lane', 'Session lane closed')
        : termination.failure)
      try {
        this.#reader.releaseLock()
      } catch {
        // Another close path may already have released the only reader.
      }
      await this.#channel.close().catch(error => this.#observeCleanup(error, 'session.channel.close'))
      this.#onClosed(this, termination)
    }
  }
}

function isFatalSessionFailure(failure: unknown): boolean {
  return failure instanceof EnvelopeError ||
    failure instanceof MessageError ||
    failure instanceof CborError ||
    (failure instanceof SessionRuntimeError && failure.scope === 'session')
}
