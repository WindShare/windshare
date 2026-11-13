import type { ReceiverSessionRuntime } from '../session/runtime'
import type { AttachedRelay, ProtocolGenerationCore, ReceiverSessionFactory } from './session-factory'
import { runGenerationRecovery } from './generation-recovery'
import type { RecoveryObservation } from './recovery-observation'
import { reconnectPhase } from './recovery-clock'

const RELAY_ATTACH_ATTEMPT_MILLISECONDS = 45_000

interface RelaySetOptions {
  readonly initial: ProtocolGenerationCore
  readonly factory: ReceiverSessionFactory
  readonly admit: (laneId: number) => void
  readonly sleep: (attempt: number, signal: AbortSignal, elapsed: number, error: unknown, relayBase: string) => Promise<void>
  readonly now: () => number
  readonly observe: (relayBase: string, observation: RecoveryObservation) => void
  readonly failure: (error: unknown) => 'retry' | 'stop'
  readonly waitForDemand?: (signal: AbortSignal) => Promise<void>
}

/** One generation owns independent relay delivery lifetimes; none may replace session authority. */
export class ReceiverRelaySet {
  readonly #options: RelaySetOptions
  readonly #session: ReceiverSessionRuntime
  readonly #connections = new Map<string, AttachedRelay>()
  readonly #tasks = new Map<string, Promise<void>>()
  readonly #disabled = new Set<string>()
  readonly #cleanup = new Set<Promise<void>>()
  readonly #lifetime = new AbortController()
  #started = false
  #closeTask: Promise<void> | undefined
  #stopCandidates: (() => void) | undefined

  constructor(options: RelaySetOptions) {
    this.#options = options
    this.#session = options.initial.session
    this.#connections.set(options.initial.relayBase, {
      relay: options.initial.relay, laneId: options.initial.relayLaneId,
      laneEpoch: options.initial.session.keys.initialLaneEpoch,
    })
  }

  start(): void {
    if (this.#started || this.#lifetime.signal.aborted) return
    this.#started = true
    for (const relayBase of this.#options.factory.relayBases) this.#ensure(relayBase)
    this.#stopCandidates = this.#options.factory.observeRelayBases?.(bases => { for (const base of bases) this.#ensure(base) })
  }

  detached(laneId: number): void {
    for (const [relayBase, attached] of this.#connections) {
      if (attached.laneId !== laneId) continue
      this.#connections.delete(relayBase)
      this.#release(attached)
      if (this.#session.availableLaneIds().length > 0) this.#ensure(relayBase)
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
      if (this.#session.availableLaneIds().length > 0 && !this.#session.isClosed) this.#ensure(relayBase)
    }).catch(() => undefined)
  }

  async #connect(relayBase: string): Promise<void> {
    let attempt = 0
    const startedAt = this.#options.now()
    const signal = this.#lifetime.signal
    while (!this.#retired && this.#session.availableLaneIds().length > 0) {
      if (attempt > 0) {
        await this.#options.waitForDemand?.(signal).catch(() => undefined)
        if (this.#retired) return
      }
      const phase = reconnectPhase(attempt, this.#options.now() - startedAt)
      attempt += 1
      this.#options.observe(relayBase, { attempt, phase, transition: 'attempt_started' })
      try {
        const attached = await runGenerationRecovery({
          reservation: { milliseconds: RELAY_ATTACH_ATTEMPT_MILLISECONDS, finish: () => undefined },
          parent: signal, now: this.#options.now,
          connect: attemptSignal => this.#options.factory.attachRelay(this.#session, attemptSignal, relayBase),
          close: attached => this.#closeAttached(attached),
        })
        if (this.#retired) {
          await this.#closeAttached(attached)
          return
        }
        await this.#admit(attached)
        this.#connections.set(relayBase, attached)
        this.#options.observe(relayBase, { attempt, phase, transition: 'connected' })
        return
      } catch (error) {
        if (!await this.#retry(relayBase, attempt, startedAt, error)) return
      }
    }
  }

  get #retired(): boolean { return this.#lifetime.signal.aborted || this.#session.isClosed }

  async #retry(relayBase: string, attempt: number, startedAt: number, error: unknown): Promise<boolean> {
    if (this.#retired) return false
    const elapsed = this.#options.now() - startedAt
    const phase = reconnectPhase(attempt - 1, elapsed)
    this.#options.observe(relayBase, { attempt, phase, transition: 'attempt_failed', failure: error })
    if (this.#options.failure(error) === 'stop') {
      this.#disabled.add(relayBase)
      this.#options.observe(relayBase, { attempt, phase, transition: 'terminal', failure: error })
      return false
    }
    await this.#options.sleep(attempt, this.#lifetime.signal, elapsed, error, relayBase).catch(() => undefined)
    return !this.#retired
  }

  async #admit(attached: AttachedRelay): Promise<void> {
    try { this.#options.admit(attached.laneId) }
    catch (error) { await this.#closeAttached(attached); throw error }
  }

  async #closeAttached(attached: AttachedRelay): Promise<void> {
    await Promise.allSettled([
      this.#session.retireLane(attached.laneId, attached.laneEpoch),
      attached.relay.close(),
    ])
  }

  #release(attached: AttachedRelay): void {
    const task = this.#closeAttached(attached)
    this.#cleanup.add(task)
    task.finally(() => this.#cleanup.delete(task)).catch(() => undefined)
  }

  async #close(): Promise<void> {
    this.#stopCandidates?.()
    this.#lifetime.abort(new DOMException('Relay generation retired', 'AbortError'))
    const connections = [...this.#connections.values()]
    this.#connections.clear()
    await Promise.allSettled([
      ...connections.map((attached) => this.#closeAttached(attached)),
      ...this.#tasks.values(),
      ...this.#cleanup,
    ])
  }
}
