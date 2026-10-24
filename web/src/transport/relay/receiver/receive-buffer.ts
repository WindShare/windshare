import type { Frame } from '../../../contracts/channel'
import { encodeDeliveryConsumed } from '../delivery-consumed-codec'
import type { RelayReceiveCapacity } from '../delivery-handshake-codec'
import { encodeSettlementAck, type RelaySettlementNotice } from '../delivery-settlement-codec'
import { RelayReceiveLedger, type ReceiveReceipt, type ReceiveSession } from '../receive-ledger'
import { type ReceiveLedgerEvent } from '../../../contracts/relay-delivery-observation'

interface ReceivedFrame {
  readonly payload: Frame
  readonly receipt: ReceiveReceipt
}

export interface ReceiveBufferOptions {
  readonly connectionId: string
  readonly relaySessionId: Uint8Array
  readonly capacity: RelayReceiveCapacity
  readonly wakeWriter: () => void
  readonly cancel: () => void
  readonly observe?: (event: ReceiveLedgerEvent) => void
}

/** One payload queue owns receipts until actual stream handoff or retirement discard. */
export class RelayReceiveBuffer {
  readonly stream: ReadableStream<Frame>
  readonly #ledger: RelayReceiveLedger
  readonly #session: ReceiveSession
  readonly #wakeWriter: () => void
  readonly #queue: ReceivedFrame[] = []
  #controller!: ReadableStreamDefaultController<Frame>
  #pullWaiting = false
  #streamClosed = false
  #mode: 'receiving' | 'draining' | 'discarding' = 'receiving'
  #connected = true

  constructor(options: ReceiveBufferOptions) {
    this.#wakeWriter = options.wakeWriter
    this.#ledger = new RelayReceiveLedger({
      connectionId: options.connectionId, capacity: options.capacity, maxSessions: 1,
      observe: event => {
        try { options.observe?.(event) } catch { /* Diagnostics cannot own delivery. */ }
      },
    })
    this.#session = this.#ledger.open(options.relaySessionId)
    // No hidden stream prefetch: enqueue must correspond to a waiting reader.
    this.stream = new ReadableStream<Frame>({
      start: controller => { this.#controller = controller },
      pull: () => { this.#pullWaiting = true; this.#drain() },
      cancel: () => {
        this.#streamClosed = true
        this.discard()
        options.cancel()
      },
    }, { highWaterMark: 0 })
  }

  get settled(): boolean { return this.#ledger.progress(this.#session)?.phase === 'settled' }

  receiveOwned(payload: Frame, encodedBytes: number): void {
    const receipt = this.#ledger.receive(this.#session, encodedBytes)
    if (this.#mode === 'discarding') {
      this.#ledger.discard(receipt)
      return
    }
    // The decoder already detached the payload from the socket event. The ledger
    // bounds both dimensions before this queue becomes its retained owner.
    this.#queue.push({ payload, receipt })
    this.#drain()
  }

  notice(notice: RelaySettlementNotice): void {
    this.#ledger.notice(this.#session, notice)
    if (this.#mode === 'receiving') this.#mode = 'draining'
    this.#drain()
    this.#wakeWriter()
  }

  discard(): void {
    if (this.#connected) this.#ledger.retire(this.#session)
    this.#mode = 'discarding'
    while (this.#queue.length !== 0) {
      const frame = this.#queue.shift()!
      if (this.#connected) this.#ledger.discard(frame.receipt)
    }
    this.#finishStream()
    this.#wakeWriter()
  }

  /** Only the ordered socket writer may take controls; a failed send ends authority. */
  takeControl(): Uint8Array<ArrayBuffer> | undefined {
    const settlement = this.#ledger.takeSettlement(this.#session)
    if (settlement !== undefined) return encodeSettlementAck(settlement)
    const consumed = this.#ledger.takeConsumed()
    return consumed === undefined ? undefined : encodeDeliveryConsumed(consumed)
  }

  disconnect(): void {
    this.#connected = false
    this.#ledger.close()
    // Transport loss can still deliver the accepted prefix to its existing reader,
    // but those later handoffs cannot report consumption on a replacement socket.
    if (this.#mode === 'receiving') this.#mode = 'draining'
    this.#drain()
  }

  fail(reason: unknown): void {
    this.#connected = false
    this.#ledger.close()
    this.#queue.length = 0
    if (!this.#streamClosed) this.#controller.error(reason)
    this.#streamClosed = true
    this.#pullWaiting = false
  }

  #drain(): void {
    if (this.#pullWaiting && this.#queue.length !== 0) {
      const { payload, receipt } = this.#queue.shift()!
      this.#pullWaiting = false
      this.#controller.enqueue(payload)
      if (this.#connected) {
        this.#ledger.consume(receipt)
        this.#wakeWriter()
      }
    }
    if (this.#mode !== 'receiving' && this.#queue.length === 0) this.#finishStream()
  }

  #finishStream(): void {
    if (this.#streamClosed) return
    this.#streamClosed = true
    this.#pullWaiting = false
    this.#controller.close()
  }
}
