import {
  createTraceCapacityPolicy,
  type TraceCapacityPolicy,
} from './capacity'
import type {
  TraceCaptureSnapshot,
  TraceCoreStatus,
  TraceHealthSnapshot,
} from './model'
import {
  BoundedTraceRecorder,
  TraceHealthAccumulator,
  type BoundedTraceRecorderOptions,
} from './recorder'
import type {
  DomainTraceSource,
  TraceCaptureSignal,
  TraceHealthReadPort,
  TraceObserver,
  TraceScheduledTask,
} from './ports'

export interface TraceActivationStore {
  readExpiry(): number | undefined
  writeExpiry(expiresAtMilliseconds: number): void
  clear(): void
}

export type TraceActivationSnapshot =
  | Readonly<{ kind: 'off' }>
  | Readonly<{ kind: 'active'; expiresAtMilliseconds: number }>

export type TraceSwitchOptions<Event, Incident, Scope> = Omit<
  BoundedTraceRecorderOptions<Event, Incident, Scope>,
  'captureGeneration' | 'capacity' | 'health' | 'onSealed'
> & Readonly<{
  capacity?: TraceCapacityPolicy
  activationStore?: TraceActivationStore
}>

interface ActiveTraceCapture<Event, Incident, Scope> {
  readonly generation: bigint
  readonly recorder: BoundedTraceRecorder<Event, Incident, Scope>
  readonly observer: TraceObserver<Event>
}

interface TraceActivation {
  readonly expiresAtMilliseconds: number
  expiryTask?: TraceScheduledTask
}

/** Owns revocation and capture replacement; product objects only see `current`. */
export class TraceSwitch<Event, Incident, Scope>
implements DomainTraceSource<Event>, TraceHealthReadPort {
  readonly #options: TraceSwitchOptions<Event, Incident, Scope>
  readonly #capacity: TraceCapacityPolicy
  readonly #health = new TraceHealthAccumulator()
  readonly #listeners = new Set<() => void>()
  #captureGeneration = 0n
  #active: ActiveTraceCapture<Event, Incident, Scope> | undefined
  #activation: TraceActivation | undefined

  constructor(options: TraceSwitchOptions<Event, Incident, Scope>) {
    this.#options = options
    this.#capacity = createTraceCapacityPolicy(options.capacity)
    this.#restoreActivation()
  }

  subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  #changed(): void {
    for (const listener of this.#listeners) {
      try { listener() } catch {
        // Diagnostic consumers cannot interrupt capture or product workflows.
      }
    }
  }

  get current(): TraceObserver<Event> | undefined {
    const active = this.#active
    return active?.recorder.enabled === true ? active.observer : undefined
  }

  activation(): TraceActivationSnapshot {
    return this.#activation === undefined
      ? Object.freeze({ kind: 'off' })
      : Object.freeze({ kind: 'active', expiresAtMilliseconds: this.#activation.expiresAtMilliseconds })
  }

  enable(): TraceCoreStatus {
    const now = this.#readNow()
    const expiresAt = Math.min(Number.MAX_SAFE_INTEGER, now + this.#capacity.captureExpiryMs)
    const status = this.#startCapture(now, expiresAt)
    try {
      if (status.enabled) this.#options.activationStore?.writeExpiry(expiresAt)
      else this.#options.activationStore?.clear()
    } catch {
      // Denied storage must not prevent collecting evidence in the current page.
    }
    return status
  }

  #startCapture(now: number, expiresAt: number): TraceCoreStatus {
    this.#cancelTask(this.#activation?.expiryTask)
    this.#discardPriorCapture()
    const generation = this.#captureGeneration + 1n
    this.#captureGeneration = generation
    const recorder = new BoundedTraceRecorder({
      ...this.#options,
      captureGeneration: generation,
      capacity: this.#capacity,
      health: this.#health,
      onSealed: () => this.#captureSealed(generation),
    })
    this.#active = { generation, recorder, observer: event => recorder.record(event) }
    const activation: TraceActivation = { expiresAtMilliseconds: expiresAt }
    this.#activation = activation
    try {
      activation.expiryTask = this.#options.scheduler.schedule(
        expiresAt - now,
        () => this.#expire(activation),
      )
    } catch {
      this.#endActivation()
      recorder.seal('expired')
    }
    this.#changed()
    return this.status()
  }

  disable(): TraceCoreStatus {
    this.#endActivation()
    try { this.#options.activationStore?.clear() } catch {
      // Revocation of this page's observer is independent of browser storage.
    }
    this.#active?.recorder.seal('manual_disable')
    // A sealed or cleared recorder still has an independently revocable activation.
    this.#changed()
    return this.status()
  }

  clear(): void {
    const active = this.#active
    if (active === undefined) return
    if (active.recorder.enabled) {
      active.recorder.clear()
      this.#changed()
      return
    }
    this.#active = undefined
    this.#changed()
  }

  signal(signal: TraceCaptureSignal<Incident, Scope>): void {
    const before = this.#active?.recorder.state
    this.#active?.recorder.signal(signal)
    if (before !== this.#active?.recorder.state) this.#changed()
  }

  status(): TraceCoreStatus {
    const active = this.#active
    if (active === undefined) {
      return Object.freeze({
        state: 'idle',
        enabled: false,
        captureGeneration: this.#captureGeneration,
        capacity: this.#capacity,
        retainedEventCount: 0n,
        retainedEventBytes: 0n,
        incidentMarkerCount: 0n,
        health: this.#health.traceHealthSnapshot(),
      })
    }
    const snapshot = active.recorder.snapshot()
    return Object.freeze({
      state: snapshot.state,
      enabled: active.recorder.enabled,
      captureGeneration: snapshot.captureGeneration,
      ...(!active.recorder.enabled || this.#activation === undefined
        ? {}
        : { expiresAtMilliseconds: this.#activation.expiresAtMilliseconds }),
      ...(snapshot.sealReason === undefined ? {} : { sealReason: snapshot.sealReason }),
      capacity: this.#capacity,
      retainedEventCount: snapshot.retainedEventCount,
      retainedEventBytes: snapshot.retainedEventBytes,
      incidentMarkerCount: snapshot.incidentMarkerCount,
      health: snapshot.health,
    })
  }

  captureSnapshot(): TraceCaptureSnapshot<Event, Incident> | undefined {
    return this.#active?.recorder.snapshot()
  }

  traceHealthSnapshot(): TraceHealthSnapshot {
    return this.#health.traceHealthSnapshot()
  }

  #restoreActivation(): void {
    try {
      const expiresAt = this.#options.activationStore?.readExpiry()
      if (expiresAt === undefined) return
      const now = this.#readNow()
      if (!Number.isSafeInteger(expiresAt) || expiresAt <= now ||
          expiresAt - now > this.#capacity.captureExpiryMs) {
        this.#options.activationStore?.clear()
        return
      }
      // Sealing preserves evidence, while activation permits re-entry until the
      // original deadline. Neither re-entry nor clearing evidence renews it.
      if (!this.#startCapture(now, expiresAt).enabled) this.#options.activationStore?.clear()
    } catch {
      // A blocked or corrupt preference cannot interrupt receiver startup.
    }
  }

  #discardPriorCapture(): void {
    const prior = this.#active
    if (prior === undefined) return
    this.#active = undefined
    prior.recorder.seal('manual_disable')
  }

  #expire(activation: TraceActivation): void {
    if (this.#activation !== activation) return
    this.#endActivation()
    try {
      const store = this.#options.activationStore
      // A cached page's old timer must not revoke a window renewed by a newer page.
      if (store?.readExpiry() === activation.expiresAtMilliseconds) store.clear()
    } catch {
      // Restoring a stored deadline also checks expiry when cleanup is unavailable.
    }
    this.#active?.recorder.seal('expired')
    this.#changed()
  }

  #endActivation(): void {
    this.#cancelTask(this.#activation?.expiryTask)
    this.#activation = undefined
  }

  #captureSealed(generation: bigint): void {
    if (this.#active?.generation === generation) this.#changed()
  }

  #cancelTask(task: TraceScheduledTask | undefined): void {
    try {
      task?.cancel()
    } catch {
      // Activation identity keeps stale expiry callbacks from sealing a replacement.
    }
  }

  #readNow(): number {
    try {
      const value = this.#options.clock.nowMilliseconds()
      if (Number.isSafeInteger(value) && value >= 0) return value
    } catch {
      // Expiry remains bounded relative to the diagnostic clock fallback.
    }
    return 0
  }
}
