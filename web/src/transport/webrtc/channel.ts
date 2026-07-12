import {
  MAX_FRAME_BYTES,
  MIN_FRAME_BYTES,
  type ChannelState,
  type FrameChannel,
} from '../../contracts/channel'
import {
  DEFAULT_DATA_CHANNEL_FLOW_CONTROL,
  TERMINAL_ACK_CONTROL,
  TERMINAL_INTENT_CONTROL,
  type DataChannelFlowControl,
  createWindShareDataChannel,
  validateDataChannelConfiguration,
  validateFlowControl,
  validateOpenedMessageCapability,
} from './config'
import {
  WebRTCChannelClosedError,
  WebRTCChannelNotOpenError,
  WebRTCFrameBoundsError,
  WebRTCIngressOverflowError,
  WebRTCPeerProtocolError,
  WebRTCRemoteClosedError,
  WebRTCTerminalNotAcknowledgedError,
  WebRTCTransportError,
} from './errors'
import { FrameStream } from './frame-stream'
import { SendLock, throwIfAborted } from './send-lock'

const INBOUND_FRAME_CAPACITY = 32

type TerminalState = 'none' | 'local' | 'remote'

interface Deferred<T> {
  readonly promise: Promise<T>
  resolve(value: T): void
  reject(reason?: unknown): void
}

export class WebRTCFrameChannel implements FrameChannel {
  readonly #peer: RTCPeerConnection
  readonly #channel: RTCDataChannel
  readonly #flow: DataChannelFlowControl
  readonly #frameStream: FrameStream
  readonly #sendLock = new SendLock()
  readonly #opened = deferred<void>()
  readonly #done = deferred<void>()
  readonly #physicalDone = deferred<void>()
  readonly #flowWaiters = new Set<() => void>()

  readonly frames: ReadableStream<Uint8Array>
  readonly opened: Promise<void>
  readonly done: Promise<void>

  #state: ChannelState = 'connecting'
  #reason: unknown
  #terminal: TerminalState = 'none'
  #localTerminalSent = false
  #localTerminalAcknowledged = false
  #remoteTerminalPublished = false
  #remoteTerminalAckSent = false
  #terminalAcknowledgement: Deferred<void> | undefined
  #closeOperation: Promise<void> | undefined
  #physicalCloseRequested = false
  #physicalSettled = false
  #physicalFailure: unknown

  constructor(peer: RTCPeerConnection, channel: RTCDataChannel) {
    validateDataChannelConfiguration(channel)
    validateFlowControl(DEFAULT_DATA_CHANNEL_FLOW_CONTROL)
    this.#peer = peer
    this.#channel = channel
    this.#flow = DEFAULT_DATA_CHANNEL_FLOW_CONTROL
    this.#frameStream = new FrameStream(INBOUND_FRAME_CAPACITY, () => {
      this.close().catch(() => undefined)
    })
    this.frames = this.#frameStream.stream
    this.opened = this.#opened.promise
    this.done = this.#done.promise
    // Consumers may observe readiness through state/done instead of awaiting this
    // promise; attaching a sink prevents an early remote close from becoming a
    // process-level unhandled rejection.
    this.opened.catch(() => undefined)

    channel.binaryType = 'arraybuffer'
    channel.bufferedAmountLowThreshold = this.#flow.lowWaterBytes
    channel.addEventListener('bufferedamountlow', this.#onBufferedAmountLow)
    channel.addEventListener('message', this.#onMessage)
    channel.addEventListener('error', this.#onError)
    channel.addEventListener('closing', this.#onClosing)
    channel.addEventListener('close', this.#onClose)
    channel.addEventListener('open', this.#onOpen)

    if (channel.readyState === 'open') {
      this.#reconcileOpen()
    }
  }

  get state(): ChannelState {
    return this.#state
  }

  get reason(): unknown {
    return this.#reason
  }

  async send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    validateFrame(frame)
    throwIfAborted(signal)
    const owned = frame.slice()
    this.#requireSendState('none')
    const release = await this.#sendLock.acquire(signal)
    try {
      this.#requireSendState('none')
      await this.#waitForCapacity('none', signal)
      throwIfAborted(signal)
      this.#requireSendState('none')
      try {
        this.#channel.send(owned)
      } catch (error) {
        const failure = new WebRTCTransportError('send binary frame', error)
        this.#finish(failure, true)
        throw failure
      }
    } finally {
      release()
    }
  }

  sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    try {
      validateFrame(frame)
      throwIfAborted(signal)
      this.#claimLocalTerminal()
    } catch (error) {
      return Promise.reject(error)
    }
    return this.#runLocalTerminal(frame.slice(), signal)
  }

  close(): Promise<void> {
    if (this.#closeOperation !== undefined) {
      return this.#closeOperation
    }
    const operation = this.#closeAndSettle()
    this.#closeOperation = operation
    return operation
  }

  async #runLocalTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    try {
      const release = await this.#sendLock.acquire(signal)
      try {
        await this.#waitForCapacity('local', signal)
        throwIfAborted(signal)
        this.#requireSendState('local')
        this.#sendRaw(TERMINAL_INTENT_CONTROL, 'send terminal intent')

        await this.#waitForCapacity('local', signal)
        throwIfAborted(signal)
        this.#requireSendState('local')
        this.#sendRaw(frame, 'send terminal frame')
        this.#localTerminalSent = true
      } finally {
        release()
      }
      await waitForAbort(this.#terminalAcknowledgement!.promise, signal)
    } catch (error) {
      if (this.#localTerminalAcknowledged) {
        return
      }
      const failure = error instanceof WebRTCTerminalNotAcknowledgedError
        ? error
        : new WebRTCTerminalNotAcknowledgedError(error)
      this.#finish(failure, true)
      throw this.#reason ?? failure
    }
  }

  #claimLocalTerminal(): void {
    this.#requireSendState('none')
    this.#terminal = 'local'
    this.#terminalAcknowledgement = deferred<void>()
    this.#terminalAcknowledgement.promise.catch(() => undefined)
    // An ordinary send may own the serialized turn while waiting for low water.
    // Waking it makes the terminal claim visible before it can enqueue more data.
    this.#wakeFlowWaiters()
  }

  async #closeAndSettle(): Promise<void> {
    if (this.#state !== 'closed') {
      if (this.#terminal === 'none') {
        this.#finish(undefined, true)
      } else {
        await this.done
      }
    }
    this.#requestPhysicalClose()
    await this.#physicalDone.promise
    if (this.#physicalFailure !== undefined) {
      throw this.#physicalFailure
    }
  }

  #onOpen = (): void => {
    this.#reconcileOpen()
  }

  #onBufferedAmountLow = (): void => {
    this.#wakeFlowWaiters()
  }

  #onMessage = (event: MessageEvent<unknown>): void => {
    if (this.#state === 'closed') {
      return
    }
    if (this.#state === 'connecting') {
      if (this.#channel.readyState !== 'open') {
        this.#finish(
          new WebRTCPeerProtocolError('message arrived before the DataChannel opened'),
          true,
        )
        return
      }
      this.#reconcileOpen()
    }
    if (this.#state !== 'open') {
      return
    }
    if (typeof event.data === 'string') {
      this.#handleControl(event.data)
      return
    }
    const frame = snapshotBinaryMessage(event.data)
    if (frame === undefined) {
      this.#failPeer('binary message was not delivered as an ArrayBuffer')
      return
    }
    if (frame.byteLength < MIN_FRAME_BYTES || frame.byteLength > MAX_FRAME_BYTES) {
      this.#failPeer(`binary message has invalid size ${frame.byteLength}`)
      return
    }
    this.#handleBinary(frame)
  }

  #onError = (event: Event): void => {
    if (this.#state === 'closed') {
      return
    }
    const error = 'error' in event
      ? (event as Event & { readonly error?: unknown }).error
      : undefined
    const cause = error ?? new Error('unspecified DataChannel error')
    const transport = new WebRTCTransportError('receive DataChannel error', cause)
    this.#finish(this.#classifyTermination(transport), true)
  }

  #onClosing = (): void => {
    if (this.#state === 'closed') {
      return
    }
    this.#finish(this.#classifyTermination(new WebRTCRemoteClosedError()), false)
  }

  #onClose = (): void => {
    this.#settlePhysical()
    if (this.#state === 'closed') {
      return
    }
    const remoteClose = new WebRTCRemoteClosedError()
    this.#finish(this.#classifyTermination(remoteClose), false)
  }

  #reconcileOpen(): void {
    if (this.#state !== 'connecting') {
      return
    }
    try {
      if (this.#channel.readyState !== 'open') {
        throw new WebRTCChannelNotOpenError()
      }
      validateDataChannelConfiguration(this.#channel)
      validateOpenedMessageCapability(this.#peer)
      this.#state = 'open'
      this.#opened.resolve()
    } catch (error) {
      this.#finish(error, true)
    }
  }

  #handleControl(control: string): void {
    if (control === TERMINAL_INTENT_CONTROL) {
      if (this.#terminal !== 'none') {
        this.#failPeer('duplicate or conflicting terminal intent')
        return
      }
      this.#terminal = 'remote'
      this.#wakeFlowWaiters()
      return
    }
    if (control === TERMINAL_ACK_CONTROL) {
      this.#acceptLocalTerminalAcknowledgement()
      return
    }
    this.#failPeer('unknown text control')
  }

  #handleBinary(frame: Uint8Array): void {
    if (this.#terminal === 'local') {
      // Ordered traffic already emitted by the peer cannot appear after the
      // acknowledged terminal; discarding it prevents application backpressure
      // from delaying that acknowledgement.
      return
    }
    if (this.#terminal === 'remote') {
      this.#handleRemoteTerminal(frame)
      return
    }
    const result = this.#frameStream.push(frame)
    if (result === 'overflow') {
      this.#finish(new WebRTCIngressOverflowError(), true)
    }
  }

  #handleRemoteTerminal(frame: Uint8Array): void {
    if (this.#remoteTerminalPublished) {
      this.#failPeer('binary message arrived after the terminal frame')
      return
    }
    const result = this.#frameStream.pushTerminal(frame)
    if (result === 'overflow') {
      this.#finish(new WebRTCIngressOverflowError(), true)
      return
    }
    if (result === 'closed') {
      // Consumer cancellation is a local lifecycle decision, not peer ingress
      // pressure. Never acknowledge a terminal that can no longer be published.
      this.#finish(
        new WebRTCChannelClosedError(
          'WebRTC receive stream closed before the terminal frame was published',
        ),
        true,
      )
      return
    }
    this.#remoteTerminalPublished = true
    this.#sendRemoteTerminalAcknowledgement()
  }

  async #sendRemoteTerminalAcknowledgement(): Promise<void> {
    try {
      const release = await this.#sendLock.acquire()
      try {
        await this.#waitForCapacity('remote')
        this.#requireSendState('remote')
        this.#sendRaw(TERMINAL_ACK_CONTROL, 'send terminal acknowledgement')
        this.#remoteTerminalAckSent = true
      } finally {
        release()
      }
      // The terminal sender owns physical close after observing the ACK. Closing
      // here could overtake text still buffered in SCTP.
      this.#finish(undefined, false)
    } catch (error) {
      if (this.#state !== 'closed') {
        this.#finish(error, true)
      }
    }
  }

  #acceptLocalTerminalAcknowledgement(): void {
    if (
      this.#terminal !== 'local' ||
      !this.#localTerminalSent ||
      this.#localTerminalAcknowledged
    ) {
      this.#failPeer('unsolicited or duplicate terminal acknowledgement')
      return
    }
    this.#localTerminalAcknowledged = true
    this.#finish(undefined, true)
    this.#terminalAcknowledgement?.resolve()
  }

  async #waitForCapacity(required: TerminalState, signal?: AbortSignal): Promise<void> {
    this.#requireSendState(required)
    if (this.#channel.bufferedAmount < this.#flow.highWaterBytes) {
      return
    }
    while (this.#channel.bufferedAmount > this.#flow.lowWaterBytes) {
      await this.#waitForFlowEvent(signal)
      throwIfAborted(signal)
      this.#requireSendState(required)
    }
  }

  #waitForFlowEvent(signal?: AbortSignal): Promise<void> {
    throwIfAborted(signal)
    return new Promise<void>((resolve, reject) => {
      const wake = () => {
        signal?.removeEventListener('abort', aborted)
        this.#flowWaiters.delete(wake)
        resolve()
      }
      const aborted = () => {
        this.#flowWaiters.delete(wake)
        reject(signal?.reason ?? new DOMException('Operation aborted', 'AbortError'))
      }
      this.#flowWaiters.add(wake)
      signal?.addEventListener('abort', aborted, { once: true })
    })
  }

  #wakeFlowWaiters(): void {
    const waiters = [...this.#flowWaiters]
    this.#flowWaiters.clear()
    waiters.forEach((wake) => wake())
  }

  #requireSendState(required: TerminalState): void {
    if (this.#state === 'connecting') {
      throw new WebRTCChannelNotOpenError()
    }
    if (this.#state === 'closed') {
      throw this.#reason ?? new WebRTCChannelClosedError()
    }
    if (this.#terminal !== required) {
      throw new WebRTCChannelClosedError('WebRTC frame channel is terminal')
    }
  }

  #sendRaw(data: string | Uint8Array, action: string): void {
    try {
      if (typeof data === 'string') {
        this.#channel.send(data)
      } else {
        const transportBytes = new Uint8Array(data.byteLength)
        transportBytes.set(data)
        this.#channel.send(transportBytes)
      }
    } catch (error) {
      throw new WebRTCTransportError(action, error)
    }
  }

  #failPeer(detail: string): void {
    this.#finish(new WebRTCPeerProtocolError(detail), true)
  }

  #classifyTermination(base: Error): Error | undefined {
    if (this.#remoteTerminalAckSent || this.#localTerminalAcknowledged) {
      return undefined
    }
    if (this.#terminal === 'local') {
      return new WebRTCTerminalNotAcknowledgedError(base)
    }
    if (this.#terminal === 'remote' && !this.#remoteTerminalPublished) {
      return new WebRTCPeerProtocolError('terminal intent had no final frame', { cause: base })
    }
    return base
  }

  #finish(reason: unknown, closePhysical: boolean): boolean {
    if (this.#state === 'closed') {
      if (closePhysical) {
        this.#requestPhysicalClose()
      }
      return false
    }
    const wasConnecting = this.#state === 'connecting'
    const classifiedReason = this.#classifyLocalTerminalFailure(reason)
    this.#state = 'closed'
    this.#reason = classifiedReason
    this.#frameStream.close()
    this.#wakeFlowWaiters()
    const unavailable = classifiedReason ?? new WebRTCChannelClosedError()
    this.#sendLock.shutdown(unavailable)
    if (wasConnecting) {
      this.#opened.reject(unavailable)
    }
    if (this.#terminal === 'local' && !this.#localTerminalAcknowledged) {
      this.#terminalAcknowledgement?.reject(unavailable)
    }
    this.#done.resolve()
    if (closePhysical) {
      this.#requestPhysicalClose()
    }
    return true
  }

  #classifyLocalTerminalFailure(reason: unknown): unknown {
    if (
      reason === undefined ||
      this.#terminal !== 'local' ||
      this.#localTerminalAcknowledged ||
      reason instanceof WebRTCTerminalNotAcknowledgedError
    ) {
      return reason
    }
    // Every failed close after local terminal admission means the peer did not
    // acknowledge the final frame; retain the lower-level typed cause beneath
    // that lifecycle fact instead of making callback ordering choose the type.
    return new WebRTCTerminalNotAcknowledgedError(reason)
  }

  #requestPhysicalClose(): void {
    if (this.#physicalCloseRequested) {
      return
    }
    this.#physicalCloseRequested = true
    if (this.#channel.readyState === 'closed') {
      this.#settlePhysical()
      return
    }
    try {
      this.#channel.close()
    } catch (error) {
      this.#physicalFailure = new WebRTCTransportError('close DataChannel', error)
      this.#settlePhysical()
      return
    }
  }

  #settlePhysical(): void {
    if (this.#physicalSettled) {
      return
    }
    this.#physicalSettled = true
    this.#physicalDone.resolve()
  }
}

export function createWindShareFrameChannel(
  peer: RTCPeerConnection,
): WebRTCFrameChannel {
  const channel = createWindShareDataChannel(peer)
  try {
    return new WebRTCFrameChannel(peer, channel)
  } catch (error) {
    try {
      channel.close()
    } catch {
      // The configuration failure is the actionable construction result; a
      // best-effort cleanup failure must not replace it with a second error.
    }
    throw error
  }
}

export function wrapWindShareDataChannel(
  peer: RTCPeerConnection,
  channel: RTCDataChannel,
): WebRTCFrameChannel {
  return new WebRTCFrameChannel(peer, channel)
}

function validateFrame(frame: Uint8Array): void {
  if (frame.byteLength < MIN_FRAME_BYTES || frame.byteLength > MAX_FRAME_BYTES) {
    throw new WebRTCFrameBoundsError(frame.byteLength, MAX_FRAME_BYTES)
  }
}

function snapshotBinaryMessage(data: unknown): Uint8Array | undefined {
  if (data instanceof ArrayBuffer) {
    return new Uint8Array(data.slice(0))
  }
  if (ArrayBuffer.isView(data)) {
    return new Uint8Array(
      data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength),
    )
  }
  return undefined
}

function deferred<T = void>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((accept, decline) => {
    resolve = accept
    reject = decline
  })
  return { promise, resolve, reject }
}

async function waitForAbort<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  throwIfAborted(signal)
  if (signal === undefined) {
    return promise
  }
  return new Promise<T>((resolve, reject) => {
    const aborted = () => reject(
      signal.reason ?? new DOMException('Operation aborted', 'AbortError'),
    )
    signal.addEventListener('abort', aborted, { once: true })
    promise.then(
      (value) => {
        signal.removeEventListener('abort', aborted)
        resolve(value)
      },
      (reason) => {
        signal.removeEventListener('abort', aborted)
        reject(reason)
      },
    )
  })
}
