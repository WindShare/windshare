import type { FrameChannel, ChannelState } from '../../src/contracts/channel'
import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import type { V2ReceiverSessionOptions } from '../../src/session/v2-runtime-types'
import type { V2SessionKeys } from '../../src/session/v2-transcript'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'
import { V2EnvelopeOpener } from '../../src/session/v2-envelope'
import { decodeV2Message } from '../../src/session/v2-message'

export function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, fail) => { resolve = accept; reject = fail })
  return { promise, resolve, reject }
}

export function id(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

export const SHARE: V2ShareDescriptor = {
  wireVersion: 2, suite: 2, shareInstance: id(1), shareInstanceId: 'share',
  syntheticRoot: id(2), syntheticRootId: 'root', chunkSize: 65_536,
  capabilities: 0n, senderPublicKey: new Uint8Array(32).fill(3),
  createdAtSeconds: 1n, pathPolicy: V2_PATH_POLICY,
}
export const KEY = new Uint8Array(32).fill(4)
export const BINDING = {
  shareInstance: SHARE.shareInstance, protocolSessionId: id(5),
  laneId: 1, laneEpoch: 0, direction: 0 as const,
}

export class BackpressuredChannel implements FrameChannel {
  state: ChannelState = 'open'
  readonly sent: Uint8Array[] = []
  sending = deferred<void>()
  readonly frames: ReadableStream<Uint8Array>
  #capacity = deferred<void>()
  #controller!: ReadableStreamDefaultController<Uint8Array>
  #failure: unknown

  constructor() {
    this.frames = new ReadableStream({ start: controller => { this.#controller = controller } })
    this.#capacity.promise.catch(() => undefined)
  }

  async send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    this.sending.resolve()
    const aborted = deferred<void>()
    const abort = () => aborted.reject(signal?.reason)
    signal?.addEventListener('abort', abort, { once: true })
    try {
      await Promise.race([this.#capacity.promise, aborted.promise])
      signal?.throwIfAborted()
      if (this.state === 'closed') throw this.#failure ?? new Error('Channel closed')
      this.sent.push(frame.slice())
    } finally {
      signal?.removeEventListener('abort', abort)
    }
  }

  sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    return this.send(frame, signal)
  }

  unblock(): void { this.#capacity.resolve() }

  block(): void {
    this.#capacity = deferred<void>()
    this.#capacity.promise.catch(() => undefined)
    this.sending = deferred<void>()
  }

  async close(): Promise<void> {
    if (this.state === 'closed') return
    this.state = 'closed'
    this.#failure = new Error('Channel closed')
    this.#capacity.reject(this.#failure)
    try { this.#controller.close() } catch { /* Reader cancellation already closed the stream. */ }
  }
}

export function runtimeFixture() {
  const channel = new BackpressuredChannel()
  const events: V2ProtocolTraceEvent[] = []
  let seed = 10
  const options: V2ReceiverSessionOptions = {
    descriptor: SHARE, readSecret: new Uint8Array(32), initialChannel: channel,
    randomBytes: length => new Uint8Array(length).fill(++seed),
    protocolTrace: { current: event => events.push(event) },
  }
  const keys: V2SessionKeys = {
    protocolSessionId: BINDING.protocolSessionId, transcriptHash: new Uint8Array(32),
    receiverToSenderKey: KEY.slice(), senderToReceiverKey: KEY.slice(),
    initialLaneId: 1, initialLaneEpoch: 0,
  }
  const Runtime = V2ReceiverSessionRuntime as unknown as new (
    options: V2ReceiverSessionOptions, keys: V2SessionKeys,
    receiverInstanceId: Uint8Array, reader: ReadableStreamDefaultReader<Uint8Array>,
  ) => V2ReceiverSessionRuntime
  return { runtime: new Runtime(options, keys, id(6), channel.frames.getReader()), channel, events }
}

export async function openSent(channel: BackpressuredChannel) {
  const opener = new V2EnvelopeOpener(KEY, BINDING)
  const messages = []
  for (const frame of channel.sent) {
    const envelope = await opener.open(frame)
    messages.push({ sequence: envelope.sequence, message: decodeV2Message(envelope.plaintext) })
  }
  return messages
}
