import { createEd25519Verifier } from '../../src/crypto/ed25519'
import type { FrameChannel, ChannelState } from '../../src/contracts/channel'
import { PATH_POLICY, type ShareDescriptor } from '../../src/catalog/records'
import { ReceiverSessionRuntime } from '../../src/session/runtime'
import type { ReceiverSessionOptions } from '../../src/session/runtime-types'
import type { SessionKeys } from '../../src/session/transcript'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import { EnvelopeOpener } from '../../src/session/envelope'
import { decodeMessage } from '../../src/session/message'

export function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, fail) => { resolve = accept; reject = fail })
  return { promise, resolve, reject }
}

export function id(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

export const SHARE: ShareDescriptor = {
  wireVersion: 2, suite: 2, shareInstance: id(1), shareInstanceId: 'share',
  syntheticRoot: id(2), syntheticRootId: 'root', chunkSize: 65_536,
  capabilities: 0n, sender: createEd25519Verifier(new Uint8Array(32).fill(3)),
  createdAtSeconds: 1n, pathPolicy: PATH_POLICY,
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

  failIncoming(reason: unknown): void { this.#controller.error(reason) }

  failOutgoing(reason: unknown): void { this.#capacity.reject(reason) }

  receive(frame: Uint8Array): void { this.#controller.enqueue(frame) }

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
  const events: ProtocolTraceEvent[] = []
  let seed = 10
  const options: ReceiverSessionOptions = {
    descriptor: SHARE, readSecret: new Uint8Array(32), initialChannel: channel,
    randomBytes: length => new Uint8Array(length).fill(++seed),
    protocolTrace: { current: event => events.push(event) },
  }
  const keys: SessionKeys = {
    protocolSessionId: BINDING.protocolSessionId, transcriptHash: new Uint8Array(32),
    receiverToSenderKey: KEY.slice(), senderToReceiverKey: KEY.slice(),
    initialLaneId: 1, initialLaneEpoch: 0,
  }
  const Runtime = ReceiverSessionRuntime as unknown as new (
    options: ReceiverSessionOptions, keys: SessionKeys,
    receiverInstanceId: Uint8Array, reader: ReadableStreamDefaultReader<Uint8Array>,
  ) => ReceiverSessionRuntime
  return { runtime: new Runtime(options, keys, id(6), channel.frames.getReader()), channel, events }
}

export async function openSent(channel: BackpressuredChannel) {
  const opener = new EnvelopeOpener(KEY, BINDING)
  const messages = []
  for (const frame of channel.sent) {
    const envelope = await opener.open(frame)
    messages.push({ sequence: envelope.sequence, message: decodeMessage(envelope.plaintext) })
  }
  return messages
}
