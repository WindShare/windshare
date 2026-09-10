import type { FrameChannel } from '../contracts/channel'
import { equalBytes } from '../crypto/bytes'
import { V2_MESSAGE_KIND, type V2SessionMessage } from './v2-message'
import { V2SessionRuntimeError } from './v2-runtime-types'

export const V2_SESSION_CONTROL_QUEUE = 256
export const V2_SESSION_DATA_QUEUE = 32
export const V2_SESSION_SEND_TIMEOUT_MILLISECONDS = 30_000

type OutboundPriority = 'control' | 'data' | 'terminal'
type OutboundPhase = 'queued' | 'sealing' | 'sending' | 'sent' | 'withdrawn' | 'failed'
export type V2SendTransition =
  | 'send_queued' | 'send_sealing' | 'send_sending' | 'send_completed'
  | 'send_withdrawn' | 'send_abandoned' | 'send_failed'

interface EnvelopeSealer {
  seal(plaintext: Uint8Array): Promise<Uint8Array<ArrayBuffer>>
}

export interface V2OutboundSend {
  readonly completion: Promise<void>
  /** Only an unsealed withdrawal proves the peer cannot observe this message. */
  cancel(reason: unknown): 'withdrawn' | 'committed'
}

interface OutboundItem {
  readonly message: V2SessionMessage
  readonly priority: OutboundPriority
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
  phase: OutboundPhase
  abandoned: boolean
}

/** Owns ordered delivery independently of the lifetime of each caller. */
export class V2SessionWriter {
  readonly #channel: FrameChannel
  readonly #sealer: EnvelopeSealer
  readonly #onFailure: (reason: unknown) => void
  readonly #observe: ((message: V2SessionMessage, transition: V2SendTransition) => void) | undefined
  readonly #control: OutboundItem[] = []
  readonly #data: OutboundItem[] = []
  readonly #lifetime = new AbortController()
  #active: OutboundItem | undefined
  #deliveryTimer: ReturnType<typeof setTimeout> | undefined
  #running = false
  #terminal = false

  constructor(channel: FrameChannel, sealer: EnvelopeSealer, options: {
    readonly onFailure: (reason: unknown) => void
    readonly observe?: (message: V2SessionMessage, transition: V2SendTransition) => void
  }) {
    this.#channel = channel
    this.#sealer = sealer
    this.#onFailure = options.onFailure
    this.#observe = options.observe
  }

  enqueue(message: V2SessionMessage, priority: OutboundPriority = 'control'): V2OutboundSend {
    this.#lifetime.signal.throwIfAborted()
    if (this.#terminal) {
      throw new V2SessionRuntimeError('session', 'Writer accepted its terminal')
    }
    const queue = priority === 'data' ? this.#data : this.#control
    const limit = priority === 'data' ? V2_SESSION_DATA_QUEUE : V2_SESSION_CONTROL_QUEUE
    if (queue.length >= limit) {
      throw new V2SessionRuntimeError('lane', 'Session writer queue is full')
    }
    if (priority === 'terminal') this.#terminal = true
    let item!: OutboundItem
    const completion = new Promise<void>((resolve, reject) => {
      item = { message, priority, resolve, reject, phase: 'queued', abandoned: false }
      queue.push(item)
    })
    this.#trace(item, 'send_queued')
    this.#run()
    return {
      completion,
      cancel: reason => this.#cancel(item, queue, reason),
    }
  }

  async send(message: V2SessionMessage, options: {
    readonly priority?: OutboundPriority
    readonly signal?: AbortSignal
  } = {}): Promise<void> {
    options.signal?.throwIfAborted()
    const delivery = this.enqueue(message, options.priority)
    const abort = () => delivery.cancel(options.signal?.reason)
    options.signal?.addEventListener('abort', abort, { once: true })
    if (options.signal?.aborted) abort()
    try {
      await delivery.completion
    } finally {
      options.signal?.removeEventListener('abort', abort)
    }
  }

  cancelPendingMessages(operationId: Uint8Array, reason: unknown): void {
    const items = [...this.#control, ...this.#data]
    if (this.#active !== undefined) items.unshift(this.#active)
    for (const item of items) {
      if (item.message.kind !== V2_MESSAGE_KIND.cancel &&
          item.message.operationId !== undefined && equalBytes(item.message.operationId, operationId)) {
        this.#cancel(item, item.priority === 'data' ? this.#data : this.#control, reason)
      }
    }
  }

  fail(reason: unknown): void {
    if (this.#lifetime.signal.aborted) return
    this.#lifetime.abort(reason)
    clearTimeout(this.#deliveryTimer)
    const items = [...this.#control.splice(0), ...this.#data.splice(0)]
    if (this.#active !== undefined) items.unshift(this.#active)
    for (const item of items) {
      item.phase = 'failed'
      item.reject(this.#lifetime.signal.reason)
      this.#trace(item, 'send_failed')
    }
  }

  #cancel(item: OutboundItem, queue: OutboundItem[], reason: unknown): 'withdrawn' | 'committed' {
    if (item.phase === 'withdrawn') return 'withdrawn'
    if (item.phase === 'queued') {
      queue.splice(queue.indexOf(item), 1)
      item.phase = 'withdrawn'
      if (item.priority === 'terminal') this.#terminal = false
      item.reject(reason)
      this.#trace(item, 'send_withdrawn')
      return 'withdrawn'
    }
    // Sealing may already consume the next sequence. Rejecting the caller must
    // neither skip this frame nor release the serialized writer turn.
    if (!item.abandoned && item.phase !== 'sent' && item.phase !== 'failed') {
      item.abandoned = true
      item.reject(reason)
      this.#trace(item, 'send_abandoned')
    }
    return 'committed'
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
    while (!this.#lifetime.signal.aborted) {
      const item = this.#control.shift() ?? this.#data.shift()
      if (item === undefined) return
      this.#active = item
      item.phase = 'sealing'
      this.#trace(item, 'send_sealing')
      // The deadline belongs to delivery, not to its possibly abandoned caller.
      // Expiry retires the lane; it never permits a later sequence on this lane.
      this.#deliveryTimer = setTimeout(() => this.#retire(new V2SessionRuntimeError(
        'lane', 'Session lane send timed out',
      )), V2_SESSION_SEND_TIMEOUT_MILLISECONDS)
      try {
        const frame = await this.#sealer.seal(item.message.plaintext)
        this.#lifetime.signal.throwIfAborted()
        item.phase = 'sending'
        this.#trace(item, 'send_sending')
        if (item.priority === 'terminal') await this.#channel.sendTerminal(frame, this.#lifetime.signal)
        else await this.#channel.send(frame, this.#lifetime.signal)
        this.#lifetime.signal.throwIfAborted()
        item.phase = 'sent'
        item.resolve()
        this.#trace(item, 'send_completed')
      } catch (error) {
        this.#retire(error)
        return
      } finally {
        clearTimeout(this.#deliveryTimer)
        this.#deliveryTimer = undefined
        this.#active = undefined
      }
    }
  }

  #retire(reason: unknown): void {
    if (this.#lifetime.signal.aborted) return
    this.fail(reason)
    this.#onFailure(reason)
  }

  #trace(item: OutboundItem, transition: V2SendTransition): void {
    try {
      this.#observe?.(item.message, transition)
    } catch {
      // Observers cannot change ownership of a sequence or queued frame.
    }
  }
}
