import { RelaySocketClosedError, type RelaySocket } from './socket'

export type OutboundItem =
  | { readonly type: 'binary'; readonly data: Uint8Array }
  | { readonly type: 'text'; readonly data: string }

interface TerminalItem {
  readonly item: OutboundItem
  readonly signal: AbortSignal | undefined
  readonly resolve: () => void
  readonly reject: (reason?: unknown) => void
}

type QueuedItem =
  | { readonly terminal: false; readonly item: OutboundItem }
  | ({ readonly terminal: true } & TerminalItem)

export class SessionOutbox {
  readonly #socket: RelaySocket
  readonly #capacity: number
  readonly #onFailure: (reason: unknown) => void
  readonly #queue: QueuedItem[] = []
  readonly #capacityWaiters = new Set<() => void>()
  readonly #lifecycleAbort = new AbortController()
  #ordinaryOutstanding = 0
  #pumping = false
  #terminalClaimed = false
  #closed = false

  constructor(
    socket: RelaySocket,
    capacity: number,
    onFailure: (reason: unknown) => void,
  ) {
    this.#socket = socket
    this.#capacity = capacity
    this.#onFailure = onFailure
  }

  async enqueue(item: OutboundItem, signal?: AbortSignal): Promise<void> {
    const owned = snapshotItem(item)
    while (this.#ordinaryOutstanding >= this.#capacity) {
      await this.#waitForCapacity(signal)
    }
    throwIfUnavailable(this.#closed, this.#terminalClaimed, signal)
    this.#ordinaryOutstanding += 1
    this.#queue.push({ terminal: false, item: owned })
    this.#startPump()
  }

  sendTerminal(item: OutboundItem, signal?: AbortSignal): Promise<void> {
    throwIfUnavailable(this.#closed, this.#terminalClaimed, signal)
    this.#terminalClaimed = true
    this.#notifyCapacity()
    return new Promise<void>((resolve, reject) => {
      const aborted = () => this.shutdown(
        signal?.reason ?? new DOMException('Operation aborted', 'AbortError'),
      )
      const settle = (callback: () => void) => () => {
        signal?.removeEventListener('abort', aborted)
        callback()
      }
      this.#queue.push({
        terminal: true,
        item: snapshotItem(item),
        signal,
        resolve: settle(resolve),
        reject: (reason) => {
          signal?.removeEventListener('abort', aborted)
          reject(reason)
        },
      })
      signal?.addEventListener('abort', aborted, { once: true })
      this.#startPump()
    })
  }

  shutdown(reason: unknown = new RelaySocketClosedError()): void {
    if (this.#closed) {
      return
    }
    this.#closed = true
    this.#terminalClaimed = true
    this.#lifecycleAbort.abort(reason)
    for (const queued of this.#queue) {
      if (queued.terminal) {
        queued.reject(reason)
      }
    }
    this.#queue.length = 0
    this.#ordinaryOutstanding = 0
    this.#notifyCapacity()
  }

  async #waitForCapacity(signal?: AbortSignal): Promise<void> {
    throwIfUnavailable(this.#closed, this.#terminalClaimed, signal)
    await new Promise<void>((resolve, reject) => {
      const available = () => {
        signal?.removeEventListener('abort', aborted)
        resolve()
      }
      const aborted = () => {
        this.#capacityWaiters.delete(available)
        reject(signal?.reason ?? new DOMException('Operation aborted', 'AbortError'))
      }
      this.#capacityWaiters.add(available)
      signal?.addEventListener('abort', aborted, { once: true })
    })
    throwIfUnavailable(this.#closed, this.#terminalClaimed, signal)
  }

  #startPump(): void {
    if (this.#pumping) {
      return
    }
    this.#pumping = true
    this.#pump().catch((error) => {
      this.shutdown(error)
      this.#onFailure(error)
    })
  }

  async #pump(): Promise<void> {
    try {
      while (!this.#closed) {
        const queued = this.#queue.shift()
        if (queued === undefined) {
          return
        }
        try {
          const signal = queued.terminal && queued.signal !== undefined
            ? AbortSignal.any([this.#lifecycleAbort.signal, queued.signal])
            : this.#lifecycleAbort.signal
          await sendItem(this.#socket, queued.item, signal)
        } catch (error) {
          if (queued.terminal) {
            queued.reject(error)
          }
          this.shutdown(error)
          this.#onFailure(error)
          return
        }
        if (queued.terminal) {
          queued.resolve()
          this.#closed = true
          return
        }
        this.#ordinaryOutstanding -= 1
        this.#notifyCapacity()
      }
    } finally {
      this.#pumping = false
    }
  }

  #notifyCapacity(): void {
    const waiters = [...this.#capacityWaiters]
    this.#capacityWaiters.clear()
    waiters.forEach((resolve) => resolve())
  }
}

function snapshotItem(item: OutboundItem): OutboundItem {
  return item.type === 'binary' ? { type: 'binary', data: item.data.slice() } : item
}

function throwIfUnavailable(
  closed: boolean,
  terminal: boolean,
  signal?: AbortSignal,
): void {
  if (signal?.aborted === true) {
    throw signal.reason ?? new DOMException('Operation aborted', 'AbortError')
  }
  if (closed || terminal) {
    throw new RelaySocketClosedError('relay session is terminal')
  }
}

async function sendItem(
  socket: RelaySocket,
  item: OutboundItem,
  signal?: AbortSignal,
): Promise<void> {
  if (item.type === 'binary') {
    await socket.sendBinary(item.data, signal)
  } else {
    await socket.sendText(item.data, signal)
  }
}
