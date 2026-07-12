import type {
  ChannelState,
  FrameChannel,
} from '../../src/contracts/channel'
import {
  createChunkSet,
  type ChunkIndex,
  type TransferPlan,
} from '../../src/contracts/selection'
import type { BlockSink, DeliveryOrder } from '../../src/contracts/sink'
import { decodeFrame } from '../../src/session'

export class MockFrameChannel implements FrameChannel {
  readonly sent: Uint8Array[] = []
  readonly terminals: Uint8Array[] = []
  readonly frames: ReadableStream<Uint8Array>
  sendHook: ((frame: Uint8Array, signal?: AbortSignal) => Promise<void>) | undefined
  closeHook: (() => Promise<void>) | undefined
  #controller!: ReadableStreamDefaultController<Uint8Array>
  #state: ChannelState
  #streamClosed = false

  constructor(state: ChannelState = 'open') {
    this.#state = state
    this.frames = new ReadableStream({
      start: (controller) => {
        this.#controller = controller
      },
    })
  }

  get state(): ChannelState {
    return this.#state
  }

  setState(state: ChannelState): void {
    this.#state = state
  }

  async send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    const owned = frame.slice()
    this.sent.push(owned)
    await this.sendHook?.(owned, signal)
  }

  async sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    this.terminals.push(frame.slice())
    await this.close()
  }

  async close(): Promise<void> {
    await this.closeHook?.()
    this.#shut()
  }

  #shut(): void {
    this.#state = 'closed'
    if (!this.#streamClosed) {
      this.#streamClosed = true
      this.#controller.close()
    }
  }

  push(frame: Uint8Array): void {
    this.#controller.enqueue(frame.slice())
  }

  remoteClose(): void {
    this.#shut()
  }
}

export class TestSink implements BlockSink {
  readonly deliveryOrder: DeliveryOrder
  readonly writes: Array<{ index: ChunkIndex; plaintext: Uint8Array }> = []
  readonly held = new Set<number>()
  finalized = false
  abortReason: unknown
  writeHook: ((index: ChunkIndex, plaintext: Uint8Array) => Promise<void>) | undefined
  finalizeHook: (() => Promise<void>) | undefined
  abortHook: ((reason: unknown) => Promise<void>) | undefined

  constructor(deliveryOrder: DeliveryOrder = 'any') {
    this.deliveryOrder = deliveryOrder
  }

  has(index: ChunkIndex): boolean {
    return this.held.has(index)
  }

  async writeBlock(index: ChunkIndex, plaintext: Uint8Array): Promise<void> {
    await this.writeHook?.(index, plaintext)
    this.writes.push({ index, plaintext: plaintext.slice() })
    this.held.add(index)
  }

  async finalize(): Promise<void> {
    await this.finalizeHook?.()
    this.finalized = true
  }

  async abort(reason: unknown): Promise<void> {
    await this.abortHook?.(reason)
    this.abortReason = reason
  }
}

export interface Gate {
  readonly promise: Promise<void>
  open(): void
}

export function gate(): Gate {
  let open!: () => void
  const promise = new Promise<void>((resolve) => {
    open = resolve
  })
  return { promise, open }
}

export function transferPlan(first: number, end: number): TransferPlan {
  return {
    planId: new Uint8Array(32),
    selectedEntries: [],
    selectedBytes: 0,
    chunks: createChunkSet([{ first, end }]),
  } as unknown as TransferPlan
}

export function sentRequest(channel: MockFrameChannel, offset = 0): number[] {
  const frame = channel.sent[offset]
  if (frame === undefined) {
    throw new Error(`missing request ${offset}`)
  }
  const decoded = decodeFrame(frame)
  if (decoded.type !== 'request') {
    throw new Error('outbound frame is not a request')
  }
  return decoded.indices.map(Number)
}

export async function settle(turns = 8): Promise<void> {
  for (let turn = 0; turn < turns; turn += 1) {
    await Promise.resolve()
  }
}
