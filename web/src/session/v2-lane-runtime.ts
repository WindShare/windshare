import type { V2ShareDescriptor } from '../catalog/v2-records'
import type { FrameChannel } from '../contracts/channel'
import { V2CborError } from '../protocol/cbor'
import { V2EnvelopeError, V2EnvelopeOpener, V2EnvelopeSealer } from './v2-envelope'
import {
  decodeV2Message,
  V2MessageError,
  type V2SessionMessage,
  verifyV2SenderControl,
} from './v2-message'
import type { V2OperationRouter } from './v2-operation-router'
import { V2SessionRuntimeError } from './v2-runtime-types'
import type { V2SessionKeys } from './v2-transcript'

export const V2_SESSION_CONTROL_QUEUE = 256
export const V2_SESSION_DATA_QUEUE = 32

type OutboundPriority = 'control' | 'data' | 'terminal'

interface OutboundItem {
  readonly message: V2SessionMessage
  readonly priority: OutboundPriority
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
}

export class V2SessionWriter {
  readonly #channel: FrameChannel
  readonly #sealer: V2EnvelopeSealer
  readonly #control: OutboundItem[] = []
  readonly #data: OutboundItem[] = []
  #running = false
  #terminal = false
  #failure: unknown

  constructor(channel: FrameChannel, sealer: V2EnvelopeSealer) {
    this.#channel = channel
    this.#sealer = sealer
  }

  send(message: V2SessionMessage, priority: OutboundPriority = 'control'): Promise<void> {
    if (this.#failure !== undefined) return Promise.reject(this.#failure)
    if (this.#terminal) {
      return Promise.reject(new V2SessionRuntimeError('session', 'Writer accepted its terminal'))
    }
    const queue = priority === 'data' ? this.#data : this.#control
    const limit = priority === 'data' ? V2_SESSION_DATA_QUEUE : V2_SESSION_CONTROL_QUEUE
    if (queue.length >= limit) {
      return Promise.reject(new V2SessionRuntimeError('lane', 'Session writer queue is full'))
    }
    if (priority === 'terminal') this.#terminal = true
    const result = new Promise<void>((resolve, reject) => {
      queue.push({ message, priority, resolve, reject })
    })
    this.#run()
    return result
  }

  fail(reason: unknown): void {
    if (this.#failure !== undefined) return
    this.#failure = reason
    for (const item of [...this.#control.splice(0), ...this.#data.splice(0)]) item.reject(reason)
  }

  #run(): void {
    if (this.#running) return
    this.#running = true
    this.#drain().finally(() => {
      this.#running = false
      if (this.#control.length > 0 || this.#data.length > 0) this.#run()
    }).catch(() => undefined)
  }

  async #drain(): Promise<void> {
    while (this.#failure === undefined) {
      const item = this.#control.shift() ?? this.#data.shift()
      if (item === undefined) return
      try {
        const frame = await this.#sealer.seal(item.message.plaintext)
        if (item.priority === 'terminal') await this.#channel.sendTerminal(frame)
        else await this.#channel.send(frame)
        item.resolve()
      } catch (error) {
        item.reject(error)
        this.fail(error)
        return
      }
    }
  }
}

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
        await this.#router.route(message.data
          ? message
          : Object.freeze({ ...message, body: authenticatedBody }))
      }
    } catch (error) {
      failure = error
      this.writer.fail(error)
    } finally {
      this.#closed = true
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
