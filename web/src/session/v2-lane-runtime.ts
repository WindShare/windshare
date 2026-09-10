import type { V2ShareDescriptor } from '../catalog/v2-records'
import type { FrameChannel } from '../contracts/channel'
import { V2CborError } from '../protocol/cbor'
import { V2EnvelopeError, V2EnvelopeOpener, V2EnvelopeSealer } from './v2-envelope'
import { protocolMessageKindV1, type V2ProtocolTraceSource } from './v2-diagnostics'
import { createV2ProtocolOperationIdentity, createV2ProtocolSessionIdentity } from './v2-identities'
import { V2SessionWriter } from './v2-writer'
export * from './v2-writer'
import {
  decodeV2Message,
  V2MessageError,
  verifyV2SenderControl,
} from './v2-message'
import type { V2OperationRouter } from './v2-operation-router'
import { V2SessionRuntimeError } from './v2-runtime-types'
import type { V2SessionKeys } from './v2-transcript'

export class V2SessionLane {
  readonly id: number
  readonly epoch: number
  readonly writer: V2SessionWriter
  readonly #channel: FrameChannel
  readonly #reader: ReadableStreamDefaultReader<Uint8Array>
  readonly #opener: V2EnvelopeOpener
  readonly #router: V2OperationRouter
  readonly #descriptor: V2ShareDescriptor
  readonly #sessionId: Uint8Array<ArrayBuffer>
  readonly #onClosed: (lane: V2SessionLane, failure: unknown, fatal: boolean) => void
  readonly #pumpTask: Promise<void>
  #closed = false
  #sendFailure: unknown
  #closeTask: Promise<void> | undefined

  constructor(options: {
    readonly channel: FrameChannel
    readonly reader: ReadableStreamDefaultReader<Uint8Array>
    readonly keys: V2SessionKeys
    readonly descriptor: V2ShareDescriptor
    readonly laneId: number
    readonly laneEpoch: number
    readonly router: V2OperationRouter
    readonly onClosed: (lane: V2SessionLane, failure: unknown, fatal: boolean) => void
    readonly protocolTrace?: V2ProtocolTraceSource
  }) {
    this.id = options.laneId
    this.epoch = options.laneEpoch
    this.#channel = options.channel
    this.#reader = options.reader
    this.#router = options.router
    this.#descriptor = options.descriptor
    this.#sessionId = options.keys.protocolSessionId.slice()
    this.#onClosed = options.onClosed
    this.writer = new V2SessionWriter(
      options.channel,
      new V2EnvelopeSealer(options.keys.receiverToSenderKey, {
        shareInstance: options.descriptor.shareInstance,
        protocolSessionId: options.keys.protocolSessionId,
        laneId: options.laneId,
        laneEpoch: options.laneEpoch,
        direction: 0,
      }),
      {
        onFailure: reason => {
          this.#sendFailure = new V2SessionRuntimeError('lane', 'Session lane delivery failed', { cause: reason })
          this.close().catch(() => undefined)
        },
        observe: (message, transition) => options.protocolTrace?.current?.({
          eventName: 'protocol_operation',
          transition,
          requestKind: protocolMessageKindV1(message.kind),
          correlation: Object.freeze({
            protocolSessionId: createV2ProtocolSessionIdentity(this.#sessionId),
            ...(message.operationId === undefined ? {} : {
              protocolOperationId: createV2ProtocolOperationIdentity(message.operationId),
            }),
            lane: Object.freeze({ id: this.id, epoch: this.epoch }),
          }),
        }),
      },
    )
    this.#opener = new V2EnvelopeOpener(options.keys.senderToReceiverKey, {
      shareInstance: options.descriptor.shareInstance,
      protocolSessionId: options.keys.protocolSessionId,
      laneId: options.laneId,
      laneEpoch: options.laneEpoch,
      direction: 1,
    })
    this.#pumpTask = this.#pump()
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #close(): Promise<void> {
    if (!this.#closed) {
      this.#closed = true
      this.writer.fail(new V2SessionRuntimeError('lane', 'Session lane closed'))
      await this.#reader.cancel().catch(() => undefined)
    }
    try {
      this.#reader.releaseLock()
    } catch {
      // The reader pump may have observed the cancellation first.
    }
    await this.#channel.close()
    await this.#pumpTask
  }

  async #pump(): Promise<void> {
    let failure: unknown
    try {
      while (!this.#closed) {
        const result = await this.#reader.read()
        if (result.done) break
        const opened = await this.#opener.open(result.value)
        const message = decodeV2Message(opened.plaintext)
        const authenticatedBody = await verifyV2SenderControl(
          message,
          {
            shareInstance: this.#descriptor.shareInstance,
            protocolSessionId: this.#sessionId,
            laneId: this.id,
            laneEpoch: this.epoch,
            direction: 1,
            sequence: opened.sequence,
          },
          this.#descriptor.senderPublicKey,
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
      failure = error
      this.writer.fail(error)
    } finally {
      this.#closed = true
      failure ??= this.#sendFailure
      if (failure === undefined) {
        this.writer.fail(new V2SessionRuntimeError('lane', 'Session lane became unavailable'))
      }
      try {
        this.#reader.releaseLock()
      } catch {
        // Another close path may already have released the only reader.
      }
      await this.#channel.close().catch(() => undefined)
      this.#onClosed(this, failure, isFatalSessionFailure(failure))
    }
  }
}

function isFatalSessionFailure(failure: unknown): boolean {
  return failure instanceof V2EnvelopeError ||
    failure instanceof V2MessageError ||
    failure instanceof V2CborError ||
    (failure instanceof V2SessionRuntimeError && failure.scope === 'session')
}
