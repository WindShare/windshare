import { bytesToHex, equalBytes } from '../../../crypto/bytes'
import type { RelayDeliveryConsumed } from '../delivery-consumed-codec'
import type { RelaySettlementAck, RelaySettlementNotice } from '../delivery-settlement-codec'
import { V2_RELAY_SESSION_ID_BYTES } from '../v2-protocol'
import {
  addCounts, appendEnvelope, positiveUint64, sameCounts, subtractCounts, ZERO_COUNTS,
} from './counts'
import { ReceiveLedgerError, type ReceiveLedgerConfig, type ReceiveReceipt, type ReceiveSession } from './types'
import { type ReceiveAction, type ReceiveCounts, type ReceiveFailure, type ReceiveProgress, type ReceiveUsage } from '../../../contracts/relay-delivery-observation'

export { ReceiveLedgerError } from './types'
export type { ReceiveCounts, ReceiveLedgerEvent, ReceiveProgress, ReceiveUsage } from '../../../contracts/relay-delivery-observation'
export type { ReceiveLedgerConfig, ReceiveReceipt, ReceiveSession } from './types'

type SettlementState =
  | { readonly phase: 'active' | 'retiring' }
  | { readonly phase: 'draining' }
  | { readonly phase: 'settled'; readonly ackPending: boolean }

interface Flow {
  readonly wireId: Uint8Array
  readonly id: string
  state: SettlementState
  received: ReceiveCounts
  consumed: ReceiveCounts
  discarded: ReceiveCounts
  consumedPrefix: ReceiveCounts
  completed: ReceiveCounts
  reported: ReceiveCounts
}

interface ReceiptPrefix {
  readonly session: ReceiveSession
  readonly before: ReceiveCounts
  readonly after: ReceiveCounts
}

/**
 * One physical connection generation, independent of sockets, timers and payload storage.
 * The adapter serializes calls, retains
 * receipts with payloads and submits returned controls in order outside content credit.
 * A failed control write must close this generation; a taken control cannot be dropped.
 */
export class RelayReceiveLedger {
  readonly #connectionId: string
  readonly #capacity: ReceiveCounts
  readonly #maxSessions: number
  readonly #observe: ReceiveLedgerConfig['observe']
  readonly #sessions = new Map<ReceiveSession, Flow>()
  readonly #wireSessions = new Set<string>()
  readonly #order: Flow[] = []
  // Weak keys let the payload owner release receipts without a second retained frame queue.
  readonly #receipts = new WeakMap<ReceiveReceipt, ReceiptPrefix>()
  #held = ZERO_COUNTS
  #outstanding = ZERO_COUNTS
  #pendingConsumed = ZERO_COUNTS
  #cursor = 0
  #closed = false

  constructor(config: ReceiveLedgerConfig) {
    if (!config.connectionId.trim() || !Number.isSafeInteger(config.maxSessions) || config.maxSessions <= 0 ||
        ![config.capacity.frames, config.capacity.encodedBytes].every(positiveUint64)) {
      throw new ReceiveLedgerError('config')
    }
    this.#connectionId = config.connectionId
    this.#capacity = Object.freeze({ ...config.capacity })
    this.#maxSessions = config.maxSessions
    this.#observe = config.observe
  }

  /** Admit each non-reused wire ID explicitly; events and callbacks cannot recreate a flow. */
  open(wireId: Uint8Array): ReceiveSession {
    if (this.#closed) this.#reject('open', undefined, 'closed')
    if (wireId.byteLength !== V2_RELAY_SESSION_ID_BYTES || !wireId.some(byte => byte !== 0)) {
      this.#reject('open', undefined, 'session')
    }
    const id = bytesToHex(wireId)
    if (this.#wireSessions.has(id)) this.#reject('open', undefined, 'session')
    if (this.#sessions.size >= this.#maxSessions) this.#reject('open', undefined, 'session-limit')
    const session = Object.freeze({}) as ReceiveSession
    const flow: Flow = {
      wireId: wireId.slice(), id, state: { phase: 'active' }, received: ZERO_COUNTS,
      consumed: ZERO_COUNTS, discarded: ZERO_COUNTS, consumedPrefix: ZERO_COUNTS,
      completed: ZERO_COUNTS, reported: ZERO_COUNTS,
    }
    this.#sessions.set(session, flow)
    this.#wireSessions.add(id)
    this.#order.push(flow)
    this.#trace('open', flow)
    return session
  }

  usage(): ReceiveUsage {
    return Object.freeze({
      held: this.#held, outstanding: this.#outstanding, pendingConsumed: this.#pendingConsumed,
      retainedSessions: this.#sessions.size,
    })
  }

  progress(session: ReceiveSession): ReceiveProgress | undefined {
    const flow = this.#sessions.get(session)
    return !this.#closed && flow ? this.#progress(flow) : undefined
  }

  /** Charge the full WS2O before retaining its payload, including in-flight data after local retirement. */
  receive(session: ReceiveSession, encodedBytes: number): ReceiveReceipt {
    const flow = this.#check(session, 'receive')
    if (flow.state.phase === 'draining' || flow.state.phase === 'settled') this.#reject('receive', flow, 'frozen-prefix')
    let after: ReceiveCounts
    try {
      after = appendEnvelope(flow.received, encodedBytes)
    } catch (error) {
      if (error instanceof ReceiveLedgerError) this.#trace('receive', flow, error.code)
      throw error
    }
    const size = subtractCounts(after, flow.received)
    const outstanding = addCounts(this.#outstanding, size)
    if (outstanding.frames > this.#capacity.frames || outstanding.encodedBytes > this.#capacity.encodedBytes) {
      this.#reject('receive', flow, 'capacity')
    }
    const receipt = Object.freeze({}) as ReceiveReceipt
    this.#receipts.set(receipt, { session, before: flow.received, after })
    flow.received = after
    this.#held = addCounts(this.#held, size)
    this.#outstanding = outstanding
    return receipt
  }

  /** FIFO application handoff; cancelling one download still consumes frames on a live relay session. */
  consume(receipt: ReceiveReceipt): void { this.#complete(receipt, 'consume') }

  /** Discard is a relay-session retirement operation, never ordinary consumption. */
  discard(receipt: ReceiveReceipt): void { this.#complete(receipt, 'discard') }

  #complete(receipt: ReceiveReceipt, action: 'consume' | 'discard'): void {
    if (this.#closed) this.#reject(action, undefined, 'closed')
    const prefix = this.#receipts.get(receipt)
    if (!prefix) this.#reject(action, undefined, 'receipt')
    const flow = this.#check(prefix.session, action)
    if (!sameCounts(prefix.before, flow.completed)) this.#reject(action, flow, 'receipt')
    if (action === 'discard' && flow.state.phase === 'active') this.#reject(action, flow, 'retirement')
    const size = subtractCounts(prefix.after, prefix.before)
    flow.completed = prefix.after
    this.#held = subtractCounts(this.#held, size)
    if (action === 'discard') {
      flow.discarded = addCounts(flow.discarded, size)
      this.#trace(action, flow)
    } else {
      flow.consumed = addCounts(flow.consumed, size)
      // WS2B can describe only a consumed prefix, never skip over a discarded hole.
      if (flow.discarded.frames === 0n) {
        flow.consumedPrefix = prefix.after
        this.#pendingConsumed = addCounts(this.#pendingConsumed, size)
      }
    }
  }

  retire(session: ReceiveSession): void {
    const flow = this.#check(session, 'retire')
    if (flow.state.phase === 'active') {
      flow.state = { phase: 'retiring' }
      this.#trace('retire', flow)
    }
  }

  /** Called in socket order: matching received totals freeze arrival, not application completion. */
  notice(session: ReceiveSession, notice: RelaySettlementNotice): void {
    const flow = this.#check(session, 'notice')
    if (notice.kind !== 'settlement-notice' || !equalBytes(notice.relaySessionId, flow.wireId) ||
        notice.totalFrames !== flow.received.frames || notice.totalEncodedBytes !== flow.received.encodedBytes) {
      this.#reject('notice', flow, 'settlement')
    }
    flow.state = flow.state.phase === 'settled' ? { phase: 'settled', ackPending: true } : { phase: 'draining' }
    this.#trace('notice', flow)
  }

  /**
   * The relay may grant less than the declared capacity, even a single envelope.
   * Feedback never waits for more input; the ordered writer coalesces pending consumption.
   */
  feedbackReady(): boolean {
    return !this.#closed && this.#pendingConsumed.frames !== 0n
  }

  takeConsumed(): RelayDeliveryConsumed | undefined {
    if (!this.feedbackReady()) return undefined
    for (let checked = 0; checked < this.#order.length; checked += 1) {
      const flow = this.#order[this.#cursor]!
      this.#cursor = (this.#cursor + 1) % this.#order.length
      if (flow.state.phase === 'settled' || sameCounts(flow.consumedPrefix, flow.reported)) continue
      const delta = subtractCounts(flow.consumedPrefix, flow.reported)
      this.#outstanding = subtractCounts(this.#outstanding, delta)
      this.#pendingConsumed = subtractCounts(this.#pendingConsumed, delta)
      flow.reported = flow.consumedPrefix
      this.#trace('feedback', flow)
      return Object.freeze({
        relaySessionId: flow.wireId.slice(), totalFrames: flow.reported.frames, totalEncodedBytes: flow.reported.encodedBytes,
      })
    }
    return undefined
  }

  /** Replayed notices may repeat an exact WS2S, but only the first acknowledgement refunds the suffix. */
  takeSettlement(session: ReceiveSession): RelaySettlementAck | undefined {
    const flow = this.#sessions.get(session)
    if (this.#closed || !flow || !sameCounts(flow.completed, flow.received)) return undefined
    const state = flow.state
    if (state.phase === 'active' || state.phase === 'retiring' || (state.phase === 'settled' && !state.ackPending)) return undefined
    if (state.phase === 'draining') {
      this.#outstanding = subtractCounts(this.#outstanding, subtractCounts(flow.received, flow.reported))
      this.#pendingConsumed = subtractCounts(this.#pendingConsumed, subtractCounts(flow.consumedPrefix, flow.reported))
    }
    flow.state = { phase: 'settled', ackPending: false }
    this.#trace('settlement', flow)
    return Object.freeze({
      kind: 'settlement-ack', relaySessionId: flow.wireId.slice(),
      totalFrames: flow.received.frames, totalEncodedBytes: flow.received.encodedBytes,
    })
  }

  /** The ordered writer must own the acknowledgement before metadata can be released. */
  forget(session: ReceiveSession): void {
    const flow = this.#check(session, 'forget')
    if (flow.state.phase !== 'settled' || flow.state.ackPending) this.#reject('forget', flow, 'settlement-pending')
    this.#sessions.delete(session)
    this.#wireSessions.delete(flow.id)
    const index = this.#order.indexOf(flow)
    this.#order.splice(index, 1)
    if (this.#cursor > index) this.#cursor -= 1
    if (this.#cursor >= this.#order.length) this.#cursor = 0
    this.#trace('forget', flow)
  }

  /** Ends feedback authority without claiming consumption or physical buffer release. */
  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#trace('close')
  }

  #check(session: ReceiveSession, action: ReceiveAction): Flow {
    const flow = this.#sessions.get(session)
    if (this.#closed) this.#reject(action, flow, 'closed')
    if (!flow) this.#reject(action, undefined, 'session')
    return flow
  }

  #progress(flow: Flow): ReceiveProgress {
    return Object.freeze({
      phase: flow.state.phase, received: flow.received, consumed: flow.consumed,
      discarded: flow.discarded, consumedPrefix: flow.consumedPrefix,
    })
  }

  #trace(action: ReceiveAction, flow?: Flow, failure?: ReceiveFailure): void {
    this.#observe?.(Object.freeze({
      connectionId: this.#connectionId, capacity: this.#capacity, sessionId: flow?.id, action, failure,
      progress: flow ? this.#progress(flow) : undefined, usage: this.usage(),
    }))
  }

  #reject(action: ReceiveAction, flow: Flow | undefined, failure: ReceiveFailure): never {
    this.#trace(action, flow, failure)
    throw new ReceiveLedgerError(failure)
  }
}
