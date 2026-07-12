import type {
  RelaySocket,
  RelaySocketMessage,
  RelaySocketState,
} from '../../src/transport/relay'

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

export class FakeRelaySocket implements RelaySocket {
  readonly sentText: string[] = []
  readonly sentBinary: Uint8Array[] = []
  readonly messages: ReadableStream<RelaySocketMessage>
  sendTextHook: ((text: string, signal?: AbortSignal) => Promise<void>) | undefined
  sendBinaryHook: ((data: Uint8Array, signal?: AbortSignal) => Promise<void>) | undefined
  closeHook: (() => Promise<void>) | undefined
  closeCalls = 0
  closeError: unknown
  #state: RelaySocketState = 'open'
  #controller!: ReadableStreamDefaultController<RelaySocketMessage>
  #closedStream = false

  constructor() {
    this.messages = new ReadableStream({
      start: (controller) => {
        this.#controller = controller
      },
      cancel: () => {
        this.closeCalls += 1
        this.#state = 'closed'
        this.#closedStream = true
      },
    })
  }

  get state(): RelaySocketState {
    return this.#state
  }

  async sendText(text: string, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    this.sentText.push(text)
    await this.sendTextHook?.(text, signal)
  }

  async sendBinary(data: Uint8Array, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    const owned = data.slice()
    this.sentBinary.push(owned)
    await this.sendBinaryHook?.(owned, signal)
  }

  pushText(data: string): void {
    this.#controller.enqueue({ type: 'text', data })
  }

  pushBinary(data: Uint8Array): void {
    this.#controller.enqueue({ type: 'binary', data: data.slice() })
  }

  fail(reason: unknown): void {
    this.#state = 'closed'
    this.#closedStream = true
    this.#controller.error(reason)
  }

  async close(): Promise<void> {
    this.closeCalls += 1
    await this.closeHook?.()
    this.#state = 'closed'
    if (!this.#closedStream) {
      this.#closedStream = true
      this.#controller.close()
    }
    if (this.closeError !== undefined) {
      throw this.closeError
    }
  }
}

export async function settle(turns = 8): Promise<void> {
  for (let turn = 0; turn < turns; turn += 1) {
    await Promise.resolve()
  }
}

export async function readAll<T>(stream: ReadableStream<T>): Promise<T[]> {
  const output: T[] = []
  const reader = stream.getReader()
  try {
    while (true) {
      const result = await reader.read()
      if (result.done) {
        return output
      }
      output.push(result.value)
    }
  } finally {
    reader.releaseLock()
  }
}
