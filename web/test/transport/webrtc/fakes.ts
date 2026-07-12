import {
  DATA_CHANNEL_LABEL,
  DATA_CHANNEL_PROTOCOL,
} from '../../../src/transport/webrtc'

export type SentData = string | Uint8Array

export class FakeRTCDataChannel extends EventTarget {
  readonly label: string
  readonly protocol: string
  readonly ordered: boolean
  readonly maxPacketLifeTime: number | null
  readonly maxRetransmits: number | null
  readonly negotiated: boolean
  readonly sent: SentData[] = []
  binaryType: BinaryType = 'blob'
  bufferedAmount = 0
  bufferedAmountLowThreshold = 0
  readyState: RTCDataChannelState
  closeCalls = 0
  sendHook: ((data: SentData) => void) | undefined
  closeDispatchesEvent = true

  constructor(options: {
    readonly label?: string
    readonly protocol?: string
    readonly ordered?: boolean
    readonly maxPacketLifeTime?: number | null
    readonly maxRetransmits?: number | null
    readonly negotiated?: boolean
    readonly readyState?: RTCDataChannelState
  } = {}) {
    super()
    this.label = options.label ?? DATA_CHANNEL_LABEL
    this.protocol = options.protocol ?? DATA_CHANNEL_PROTOCOL
    this.ordered = options.ordered ?? true
    this.maxPacketLifeTime = options.maxPacketLifeTime ?? null
    this.maxRetransmits = options.maxRetransmits ?? null
    this.negotiated = options.negotiated ?? false
    this.readyState = options.readyState ?? 'open'
  }

  send(data: string | Blob | ArrayBuffer | ArrayBufferView): void {
    if (this.readyState !== 'open') {
      throw new DOMException('DataChannel is not open', 'InvalidStateError')
    }
    const owned = snapshotSentData(data)
    this.sent.push(owned)
    this.sendHook?.(owned)
  }

  close(): void {
    this.closeCalls += 1
    if (this.readyState === 'closed') {
      return
    }
    this.readyState = 'closed'
    if (this.closeDispatchesEvent) {
      this.dispatchEvent(new Event('close'))
    }
  }

  open(): void {
    this.readyState = 'open'
    this.dispatchEvent(new Event('open'))
  }

  receiveText(text: string): void {
    this.dispatchEvent(messageEvent(text))
  }

  receiveBinary(bytes: Uint8Array): void {
    this.dispatchEvent(messageEvent(bytes))
  }

  receiveUnknown(data: unknown): void {
    this.dispatchEvent(messageEvent(data))
  }

  remoteClose(): void {
    if (this.readyState === 'closed') {
      return
    }
    this.readyState = 'closed'
    this.dispatchEvent(new Event('close'))
  }

  fail(error: unknown): void {
    const event = new Event('error')
    Object.defineProperty(event, 'error', { value: error })
    this.dispatchEvent(event)
  }

  setBufferedAmount(amount: number): void {
    const prior = this.bufferedAmount
    this.bufferedAmount = amount
    if (
      prior > this.bufferedAmountLowThreshold &&
      amount <= this.bufferedAmountLowThreshold
    ) {
      this.emitBufferedAmountLow()
    }
  }

  emitBufferedAmountLow(): void {
    this.dispatchEvent(new Event('bufferedamountlow'))
  }

  asDataChannel(): RTCDataChannel {
    return this as unknown as RTCDataChannel
  }
}

export class FakeRTCPeerConnection {
  readonly raw: FakeRTCDataChannel
  readonly sctp: Pick<RTCSctpTransport, 'maxMessageSize'>
  createDataChannelCalls = 0
  createOfferCalls = 0
  lastLabel: string | undefined
  lastOptions: RTCDataChannelInit | undefined

  constructor(raw = new FakeRTCDataChannel(), maximumMessageSize = 256 * 1024) {
    this.raw = raw
    this.sctp = { maxMessageSize: maximumMessageSize }
  }

  createDataChannel(label: string, options?: RTCDataChannelInit): RTCDataChannel {
    this.createDataChannelCalls += 1
    this.lastLabel = label
    this.lastOptions = options === undefined ? undefined : { ...options }
    return this.raw.asDataChannel()
  }

  createOffer(): never {
    this.createOfferCalls += 1
    throw new Error('adapter must not create an SDP offer')
  }

  asPeer(): RTCPeerConnection {
    return this as unknown as RTCPeerConnection
  }
}

export async function settle(turns = 8): Promise<void> {
  for (let turn = 0; turn < turns; turn += 1) {
    await Promise.resolve()
  }
}

export async function readAll<T>(stream: ReadableStream<T>): Promise<T[]> {
  const frames: T[] = []
  const reader = stream.getReader()
  while (true) {
    const result = await reader.read()
    if (result.done) {
      return frames
    }
    frames.push(result.value)
  }
}

function snapshotSentData(data: string | Blob | ArrayBuffer | ArrayBufferView): SentData {
  if (typeof data === 'string') {
    return data
  }
  if (data instanceof Blob) {
    throw new TypeError('fake DataChannel does not accept Blob sends')
  }
  if (data instanceof ArrayBuffer) {
    return new Uint8Array(data.slice(0))
  }
  return new Uint8Array(
    data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength),
  )
}

function messageEvent(data: unknown): MessageEvent<unknown> {
  const event = new Event('message')
  Object.defineProperty(event, 'data', { value: data })
  return event as MessageEvent<unknown>
}
