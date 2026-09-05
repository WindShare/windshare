import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import type { V2AttachedRelay, V2ProtocolGenerationCore, V2ReceiverSessionFactory } from './v2-session-factory'

interface RelaySetOptions {
  readonly initial: V2ProtocolGenerationCore
  readonly factory: V2ReceiverSessionFactory
  readonly admit: (laneId: number) => void
  readonly sleep: (attempt: number, signal: AbortSignal) => Promise<void>
  readonly failure: (error: unknown) => 'retry' | 'stop'
}

/** One generation owns independent relay delivery lifetimes; none may replace session authority. */
export class ReceiverRelaySet {
  readonly #options: RelaySetOptions
  readonly #session: V2ReceiverSessionRuntime
  readonly #connections = new Map<string, V2AttachedRelay>()
  readonly #tasks = new Map<string, Promise<void>>()
  readonly #disabled = new Set<string>()
  readonly #cleanup = new Set<Promise<void>>()
  readonly #lifetime = new AbortController()
  #started = false
  #closeTask: Promise<void> | undefined

  constructor(options: RelaySetOptions) {
    this.#options = options
    this.#session = options.initial.session
    this.#connections.set(options.initial.relayBase, {
      relay: options.initial.relay, laneId: options.initial.relayLaneId,
    })
  }

  start(): void {
    if (this.#started || this.#lifetime.signal.aborted) return
    this.#started = true
    for (const relayBase of this.#options.factory.relayBases) this.#ensure(relayBase)
  }

  detached(laneId: number): void {
    for (const [relayBase, attached] of this.#connections) {
      if (attached.laneId !== laneId) continue
      this.#connections.delete(relayBase)
      this.#release(attached)
      if (this.#session.laneIds().length > 0) this.#ensure(relayBase)
      return
    }
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  #ensure(relayBase: string): void {
    if (this.#lifetime.signal.aborted || this.#disabled.has(relayBase) ||
      this.#connections.has(relayBase) || this.#tasks.has(relayBase)) return
    const task = this.#connect(relayBase)
    this.#tasks.set(relayBase, task)
    task.finally(() => {
      if (this.#tasks.get(relayBase) === task) this.#tasks.delete(relayBase)
      if (this.#session.laneIds().length > 0 && !this.#session.isClosed) this.#ensure(relayBase)
    }).catch(() => undefined)
  }

  async #connect(relayBase: string): Promise<void> {
    let attempt = 0
    const signal = this.#lifetime.signal
    while (!signal.aborted && !this.#session.isClosed && this.#session.laneIds().length > 0) {
      try {
        const attached = await this.#options.factory.attachRelay(this.#session, signal, relayBase)
        if (signal.aborted || this.#session.isClosed) {
          await attached.relay.close().catch(() => undefined)
          return
        }
        try {
          this.#options.admit(attached.laneId)
        } catch (error) {
          await attached.relay.close().catch(() => undefined)
          throw error
        }
        this.#connections.set(relayBase, attached)
        return
      } catch (error) {
        if (signal.aborted) return
        if (this.#options.failure(error) === 'stop') {
          this.#disabled.add(relayBase)
          return
        }
        await this.#options.sleep(attempt++, signal).catch(() => undefined)
      }
    }
  }

  #release(attached: V2AttachedRelay): void {
    const task = attached.relay.close().catch(() => undefined)
    this.#cleanup.add(task)
    task.finally(() => this.#cleanup.delete(task)).catch(() => undefined)
  }

  async #close(): Promise<void> {
    this.#lifetime.abort(new DOMException('Relay generation retired', 'AbortError'))
    const connections = [...this.#connections.values()]
    this.#connections.clear()
    await Promise.allSettled([
      ...connections.map((attached) => attached.relay.close()),
      ...this.#tasks.values(),
      ...this.#cleanup,
    ])
  }
}
