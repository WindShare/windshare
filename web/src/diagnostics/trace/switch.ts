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

export type TraceSwitchOptions<Event, Incident, Scope> = Omit<
  BoundedTraceRecorderOptions<Event, Incident, Scope>,
  'captureGeneration' | 'capacity' | 'health' | 'onSealed'
> & Readonly<{ capacity?: TraceCapacityPolicy }>

interface ActiveTraceCapture<Event, Incident, Scope> {
  readonly generation: bigint
  readonly recorder: BoundedTraceRecorder<Event, Incident, Scope>
  readonly observer: TraceObserver<Event>
  expiryTask?: TraceScheduledTask
  expiresAtMilliseconds?: number
}

/** Owns revocation and capture replacement; product objects only see `current`. */
export class TraceSwitch<Event, Incident, Scope>
implements DomainTraceSource<Event>, TraceHealthReadPort {
  readonly #options: TraceSwitchOptions<Event, Incident, Scope>
  readonly #capacity: TraceCapacityPolicy
  readonly #health = new TraceHealthAccumulator()
  #captureGeneration = 0n
  #active: ActiveTraceCapture<Event, Incident, Scope> | undefined

  constructor(options: TraceSwitchOptions<Event, Incident, Scope>) {
    this.#options = options
    this.#capacity = createTraceCapacityPolicy(options.capacity)
  }

  get current(): TraceObserver<Event> | undefined {
    const active = this.#active
    return active?.recorder.enabled === true ? active.observer : undefined
  }

  enable(): TraceCoreStatus {
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
    const active: ActiveTraceCapture<Event, Incident, Scope> = {
      generation,
      recorder,
      observer: (event) => recorder.record(event),
    }
    this.#active = active
    const now = this.#readNow()
    active.expiresAtMilliseconds = Math.min(
      Number.MAX_SAFE_INTEGER,
      now + this.#capacity.captureExpiryMs,
    )
    try {
      active.expiryTask = this.#options.scheduler.schedule(
        this.#capacity.captureExpiryMs,
        () => this.#expire(generation, recorder),
      )
    } catch {
      recorder.seal('expired')
    }
    return this.status()
  }

  disable(): TraceCoreStatus {
    this.#active?.recorder.seal('manual_disable')
    return this.status()
  }

  clear(): void {
    const active = this.#active
    if (active === undefined) return
    if (active.recorder.enabled) {
      active.recorder.clear()
      return
    }
    this.#cancelTask(active.expiryTask)
    this.#active = undefined
  }

  signal(signal: TraceCaptureSignal<Incident, Scope>): void {
    this.#active?.recorder.signal(signal)
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
      ...(active.expiresAtMilliseconds === undefined
        ? {}
        : { expiresAtMilliseconds: active.expiresAtMilliseconds }),
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

  #discardPriorCapture(): void {
    const prior = this.#active
    if (prior === undefined) return
    this.#active = undefined
    this.#cancelTask(prior.expiryTask)
    prior.recorder.seal('manual_disable')
  }

  #expire(
    generation: bigint,
    recorder: BoundedTraceRecorder<Event, Incident, Scope>,
  ): void {
    const active = this.#active
    if (generation !== this.#captureGeneration || active?.recorder !== recorder) return
    recorder.seal('expired')
  }

  #captureSealed(generation: bigint): void {
    const active = this.#active
    if (active?.generation !== generation) return
    this.#cancelTask(active.expiryTask)
    delete active.expiryTask
    delete active.expiresAtMilliseconds
  }

  #cancelTask(task: TraceScheduledTask | undefined): void {
    try {
      task?.cancel()
    } catch {
      // Generation checks keep a cancellation failure from sealing a replacement.
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
