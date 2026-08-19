import {
  createTraceCapacityPolicy,
  type TraceCapacityPolicy,
} from './capacity'
import {
  TRACE_EVENT_NAMES_V1,
  type TraceCapturedEvent,
  type TraceCaptureSnapshot,
  type TraceCaptureState,
  type TraceEventNameV1,
  type TraceHealthCounter,
  type TraceHealthSnapshot,
  type TraceSealReason,
} from './model'
import type {
  TraceCaptureSignal,
  TraceClock,
  TraceHealthReadPort,
  TraceScheduledTask,
  TraceScheduler,
} from './ports'

type DomainTraceEventName = Exclude<TraceEventNameV1, 'incident_marker'>
type CapturePhase = 'pre' | 'post'

interface StoredTraceEvent<Event, Incident> {
  readonly phase: CapturePhase
  readonly name: TraceEventNameV1
  readonly record: TraceCapturedEvent<Event, Incident>
}

interface PreparedDomainEvent<Event, Incident> {
  readonly name: DomainTraceEventName
  readonly record: TraceCapturedEvent<Event, Incident>
}

interface CoalescedEvent<Event, Incident> {
  readonly phase: CapturePhase
  readonly observedAtMilliseconds: number
  readonly stored: StoredTraceEvent<Event, Incident>
}

export interface BoundedTraceRecorderOptions<Event, Incident, Scope> {
  readonly captureGeneration: bigint
  readonly capacity?: TraceCapacityPolicy
  readonly clock: TraceClock
  readonly scheduler: TraceScheduler
  readonly eventName: (event: Event) => DomainTraceEventName
  readonly snapshotEvent: (event: Event) => Event
  readonly eventBytes: (event: Event) => number
  readonly snapshotIncident: (incident: Incident) => Incident
  readonly incidentMarkerBytes: (incident: Incident) => number
  readonly incidentScope: (incident: Incident) => Scope
  readonly sameScope: (left: Scope, right: Scope) => boolean
  readonly health?: TraceHealthAccumulator
  readonly onSealed?: (reason: TraceSealReason) => void
}

export class TraceHealthAccumulator implements TraceHealthReadPort {
  #droppedCount = 0n
  #overwrittenCount = 0n
  #sampledCount = 0n
  #coalescedCount = 0n

  increment(counter: TraceHealthCounter): void {
    switch (counter) {
      case 'droppedCount': this.#droppedCount += 1n; break
      case 'overwrittenCount': this.#overwrittenCount += 1n; break
      case 'sampledCount': this.#sampledCount += 1n; break
      case 'coalescedCount': this.#coalescedCount += 1n; break
    }
  }

  traceHealthSnapshot(): TraceHealthSnapshot {
    return Object.freeze({
      droppedCount: this.#droppedCount,
      overwrittenCount: this.#overwrittenCount,
      sampledCount: this.#sampledCount,
      coalescedCount: this.#coalescedCount,
    })
  }
}

export class BoundedTraceRecorder<Event, Incident, Scope> {
  readonly #captureGeneration: bigint
  readonly #capacity: TraceCapacityPolicy
  readonly #clock: TraceClock
  readonly #scheduler: TraceScheduler
  readonly #eventName: BoundedTraceRecorderOptions<Event, Incident, Scope>['eventName']
  readonly #snapshotEvent: BoundedTraceRecorderOptions<Event, Incident, Scope>['snapshotEvent']
  readonly #eventBytes: BoundedTraceRecorderOptions<Event, Incident, Scope>['eventBytes']
  readonly #snapshotIncident: BoundedTraceRecorderOptions<Event, Incident, Scope>['snapshotIncident']
  readonly #incidentMarkerBytes: BoundedTraceRecorderOptions<Event, Incident, Scope>['incidentMarkerBytes']
  readonly #incidentScope: BoundedTraceRecorderOptions<Event, Incident, Scope>['incidentScope']
  readonly #sameScope: BoundedTraceRecorderOptions<Event, Incident, Scope>['sameScope']
  readonly #health: TraceHealthAccumulator
  readonly #onSealed: BoundedTraceRecorderOptions<Event, Incident, Scope>['onSealed']
  readonly #startedAtMilliseconds: number
  readonly #events: StoredTraceEvent<Event, Incident>[] = []
  readonly #sampledAt = new Map<TraceEventNameV1, number>()
  readonly #coalesced = new Map<TraceEventNameV1, CoalescedEvent<Event, Incident>>()
  #state: Exclude<TraceCaptureState, 'idle'> = 'recording_pre_failure'
  #sealReason: TraceSealReason | undefined
  #lastNowMilliseconds: number
  #nextSequence = 1n
  #retainedBytes = 0
  #preFailureEventCount = 0
  #preFailureBytes = 0
  #postFailureEventCount = 0
  #postFailureBytes = 0
  #incidentMarkerCount = 0
  #rootIncidentScope: Scope | undefined
  #timerEpoch = 0
  #postFailureDeadline: TraceScheduledTask | undefined
  #silenceDeadline: TraceScheduledTask | undefined

  constructor(options: BoundedTraceRecorderOptions<Event, Incident, Scope>) {
    if (typeof options.captureGeneration !== 'bigint' || options.captureGeneration <= 0n) {
      throw new RangeError('trace capture generation must be a positive bigint')
    }
    this.#captureGeneration = options.captureGeneration
    this.#capacity = createTraceCapacityPolicy(options.capacity)
    this.#clock = options.clock
    this.#scheduler = options.scheduler
    this.#eventName = options.eventName
    this.#snapshotEvent = options.snapshotEvent
    this.#eventBytes = options.eventBytes
    this.#snapshotIncident = options.snapshotIncident
    this.#incidentMarkerBytes = options.incidentMarkerBytes
    this.#incidentScope = options.incidentScope
    this.#sameScope = options.sameScope
    this.#health = options.health ?? new TraceHealthAccumulator()
    this.#onSealed = options.onSealed
    this.#startedAtMilliseconds = this.#readNow(0)
    this.#lastNowMilliseconds = this.#startedAtMilliseconds
  }

  get state(): Exclude<TraceCaptureState, 'idle'> {
    return this.#state
  }

  get enabled(): boolean {
    return this.#state === 'recording_pre_failure' || this.#state === 'recording_post_failure'
  }

  record(event: Event): void {
    if (!this.enabled) return
    let name: DomainTraceEventName
    try {
      name = this.#requireDomainEventName(this.#eventName(event))
    } catch {
      this.#health.increment('droppedCount')
      return
    }
    if (name === 'transfer_progress') {
      this.#recordSampled(event, name)
      return
    }
    if (name === 'checkpoint') {
      this.#recordCoalesced(event, name)
      return
    }
    const prepared = this.#prepareDomainEvent(event, name)
    if (prepared === undefined) return
    this.#admitPrepared(prepared)
  }

  #recordSampled(event: Event, name: DomainTraceEventName): void {
    const now = this.#readNow(this.#lastNowMilliseconds)
    const previous = this.#sampledAt.get(name)
    if (previous !== undefined && now - previous < this.#capacity.progressSampleIntervalMs) {
      this.#health.increment('sampledCount')
      this.#notePostFailureActivity()
      return
    }
    const prepared = this.#prepareDomainEvent(event, name, now)
    if (prepared === undefined) return
    if (this.#admitPrepared(prepared) !== undefined) this.#sampledAt.set(name, now)
  }

  #recordCoalesced(event: Event, name: DomainTraceEventName): void {
    const now = this.#readNow(this.#lastNowMilliseconds)
    const previous = this.#coalesced.get(name)
    const phase = this.#phase()
    if (previous !== undefined && previous.phase === phase &&
        this.#events.includes(previous.stored) &&
        now - previous.observedAtMilliseconds < this.#capacity.checkpointCoalesceIntervalMs) {
      const prepared = this.#prepareDomainEvent(event, name, now)
      if (prepared === undefined) return
      const replacement = this.#replaceCoalesced(previous.stored, prepared)
      if (replacement !== undefined) {
        this.#health.increment('coalescedCount')
        this.#coalesced.set(name, Object.freeze({
          phase,
          observedAtMilliseconds: now,
          stored: replacement,
        }))
      }
      this.#notePostFailureActivity()
      return
    }
    const prepared = this.#prepareDomainEvent(event, name, now)
    if (prepared === undefined) return
    const stored = this.#admitPrepared(prepared)
    if (stored !== undefined) {
      this.#coalesced.set(name, Object.freeze({
        phase,
        observedAtMilliseconds: now,
        stored,
      }))
    }
  }

  signal(signal: TraceCaptureSignal<Incident, Scope>): void {
    if (!this.enabled || typeof signal.elapsedMs !== 'bigint' || signal.elapsedMs < 0n) {
      if (this.enabled) this.#health.increment('droppedCount')
      return
    }
    if (signal.kind === 'scope_terminal') {
      if (this.#state === 'recording_post_failure' && this.#rootIncidentScope !== undefined) {
        let matches = false
        try {
          matches = this.#sameScope(this.#rootIncidentScope, signal.scope)
        } catch {
          this.#health.increment('droppedCount')
        }
        if (matches) this.seal('scope_terminal')
      }
      return
    }
    this.#recordIncidentMarker(signal.incident, signal.elapsedMs)
  }

  clear(): void {
    if (!this.enabled) return
    this.#events.splice(0)
    this.#retainedBytes = 0
    this.#preFailureEventCount = 0
    this.#preFailureBytes = 0
    this.#postFailureEventCount = 0
    this.#postFailureBytes = 0
    this.#incidentMarkerCount = 0
    this.#rootIncidentScope = undefined
    this.#sampledAt.clear()
    this.#coalesced.clear()
    this.#cancelPostFailureTimers()
    this.#state = 'recording_pre_failure'
  }

  seal(reason: TraceSealReason): void {
    if (!this.enabled) return
    this.#state = 'sealed'
    this.#sealReason = reason
    this.#sampledAt.clear()
    this.#coalesced.clear()
    this.#cancelPostFailureTimers()
    try {
      this.#onSealed?.(reason)
    } catch {
      // A capture is already sealed; lifecycle notification cannot reopen it.
    }
  }

  snapshot(): TraceCaptureSnapshot<Event, Incident> {
    const events = Object.freeze(this.#events.map((entry) => entry.record))
    return Object.freeze({
      state: this.#state,
      captureGeneration: this.#captureGeneration,
      startedAtMilliseconds: this.#startedAtMilliseconds,
      ...(this.#sealReason === undefined ? {} : { sealReason: this.#sealReason }),
      retainedEventCount: BigInt(events.length),
      retainedEventBytes: BigInt(this.#retainedBytes),
      incidentMarkerCount: BigInt(this.#incidentMarkerCount),
      events,
      health: this.#health.traceHealthSnapshot(),
    })
  }

  #recordIncidentMarker(incident: Incident, elapsedMs: bigint): void {
    if (this.#incidentMarkerCount >= this.#capacity.maxIncidentMarkers) {
      this.#dropAndSealForCapacity()
      return
    }
    let snapshot: Incident
    let scope: Scope
    try {
      snapshot = this.#snapshotIncident(incident)
      scope = this.#incidentScope(snapshot)
    } catch {
      this.#health.increment('droppedCount')
      return
    }
    if (this.#state === 'recording_pre_failure') this.#beginPostFailure(scope)
    if (!this.enabled) return
    let encodedBytes: number
    try {
      encodedBytes = this.#requireEncodedBytes(this.#incidentMarkerBytes(snapshot))
    } catch {
      this.#health.increment('droppedCount')
      return
    }
    if (encodedBytes > this.#capacity.maxEventBytes) {
      this.#health.increment('droppedCount')
      return
    }
    const now = this.#readNow(this.#lastNowMilliseconds)
    const record: TraceCapturedEvent<Event, Incident> = Object.freeze({
      sequence: this.#nextSequence++,
      observedAtMilliseconds: now,
      elapsedMs,
      encodedBytes,
      value: Object.freeze({
        kind: 'incident_marker' as const,
        incident: snapshot,
        eventName: 'incident_marker' as const,
      }),
    })
    const stored: StoredTraceEvent<Event, Incident> = Object.freeze({
      phase: 'post' as const,
      name: 'incident_marker' as const,
      record,
    })
    if (!this.#canAdmitPost(encodedBytes)) {
      this.#dropAndSealForCapacity()
      return
    }
    this.#append(stored)
    this.#incidentMarkerCount += 1
    this.#notePostFailureActivity()
  }

  #beginPostFailure(scope: Scope): void {
    this.#state = 'recording_post_failure'
    this.#rootIncidentScope = scope
    this.#sampledAt.clear()
    this.#coalesced.clear()
    this.#timerEpoch += 1
    const epoch = this.#timerEpoch
    try {
      this.#postFailureDeadline = this.#scheduler.schedule(
        this.#capacity.maxPostFailureMs,
        () => {
          if (this.#timerEpoch === epoch && this.#state === 'recording_post_failure') {
            this.seal('post_failure_silence')
          }
        },
      )
      this.#armSilenceDeadline(epoch)
    } catch {
      this.seal('post_failure_silence')
    }
  }

  #prepareDomainEvent(
    input: Event,
    knownName?: DomainTraceEventName,
    knownNow?: number,
  ): PreparedDomainEvent<Event, Incident> | undefined {
    let event: Event
    let name: DomainTraceEventName
    let encodedBytes: number
    try {
      name = knownName ?? this.#requireDomainEventName(this.#eventName(input))
      event = this.#snapshotEvent(input)
      encodedBytes = this.#requireEncodedBytes(this.#eventBytes(event))
    } catch {
      this.#health.increment('droppedCount')
      return undefined
    }
    if (encodedBytes > this.#capacity.maxEventBytes) {
      this.#health.increment('droppedCount')
      return undefined
    }
    const now = knownNow ?? this.#readNow(this.#lastNowMilliseconds)
    const elapsedMs = BigInt(Math.max(0, now - this.#startedAtMilliseconds))
    return Object.freeze({
      name,
      record: Object.freeze({
        sequence: this.#nextSequence++,
        observedAtMilliseconds: now,
        elapsedMs,
        encodedBytes,
        value: Object.freeze({ kind: 'event' as const, event, eventName: name }),
      }),
    })
  }

  #admitPrepared(
    prepared: PreparedDomainEvent<Event, Incident>,
  ): StoredTraceEvent<Event, Incident> | undefined {
    const stored: StoredTraceEvent<Event, Incident> = Object.freeze({
      phase: this.#phase(),
      name: prepared.name,
      record: prepared.record,
    })
    if (stored.phase === 'pre') {
      while (!this.#canAdmitPre(stored.record.encodedBytes)) this.#overwriteOldestPreFailure()
      this.#append(stored)
      return stored
    }
    if (!this.#canAdmitPost(stored.record.encodedBytes)) {
      this.#dropAndSealForCapacity()
      return undefined
    }
    this.#append(stored)
    this.#notePostFailureActivity()
    return stored
  }

  #replaceCoalesced(
    previous: StoredTraceEvent<Event, Incident>,
    prepared: PreparedDomainEvent<Event, Incident>,
  ): StoredTraceEvent<Event, Incident> | undefined {
    const index = this.#events.indexOf(previous)
    if (index < 0) return this.#admitPrepared(prepared)
    const replacement: StoredTraceEvent<Event, Incident> = Object.freeze({
      phase: previous.phase,
      name: prepared.name,
      record: prepared.record,
    })
    const byteDelta = replacement.record.encodedBytes - previous.record.encodedBytes
    if (previous.phase === 'post' && !this.#canReplacePost(byteDelta)) {
      this.#dropAndSealForCapacity()
      return undefined
    }
    if (previous.phase === 'pre') {
      while (!this.#canReplacePre(byteDelta)) {
        const candidate = this.#events.find((entry) => entry.phase === 'pre' && entry !== previous)
        if (candidate === undefined) {
          this.#health.increment('droppedCount')
          return undefined
        }
        this.#remove(candidate)
        this.#health.increment('overwrittenCount')
      }
    }
    const currentIndex = this.#events.indexOf(previous)
    if (currentIndex < 0) return undefined
    // A coalesced observation represents the newest point in time, so keeping
    // the old array position would make sequence order contradict chronology.
    this.#events.splice(currentIndex, 1)
    this.#events.push(replacement)
    this.#retainedBytes += byteDelta
    if (previous.phase === 'pre') this.#preFailureBytes += byteDelta
    else this.#postFailureBytes += byteDelta
    return replacement
  }

  #canAdmitPre(encodedBytes: number): boolean {
    return this.#events.length + 1 <= this.#capacity.maxEventCount &&
      this.#retainedBytes + encodedBytes <= this.#capacity.maxTotalBytes &&
      this.#preFailureEventCount + 1 <= this.#capacity.maxPreFailureEventCount &&
      this.#preFailureBytes + encodedBytes <= this.#capacity.maxPreFailureBytes
  }

  #canAdmitPost(encodedBytes: number): boolean {
    return this.#events.length + 1 <= this.#capacity.maxEventCount &&
      this.#retainedBytes + encodedBytes <= this.#capacity.maxTotalBytes &&
      this.#postFailureEventCount + 1 <= this.#capacity.maxPostFailureEventCount &&
      this.#postFailureBytes + encodedBytes <= this.#capacity.maxPostFailureBytes
  }

  #canReplacePre(byteDelta: number): boolean {
    return this.#retainedBytes + byteDelta <= this.#capacity.maxTotalBytes &&
      this.#preFailureBytes + byteDelta <= this.#capacity.maxPreFailureBytes
  }

  #canReplacePost(byteDelta: number): boolean {
    return this.#retainedBytes + byteDelta <= this.#capacity.maxTotalBytes &&
      this.#postFailureBytes + byteDelta <= this.#capacity.maxPostFailureBytes
  }

  #append(stored: StoredTraceEvent<Event, Incident>): void {
    this.#events.push(stored)
    this.#retainedBytes += stored.record.encodedBytes
    if (stored.phase === 'pre') {
      this.#preFailureEventCount += 1
      this.#preFailureBytes += stored.record.encodedBytes
    } else {
      this.#postFailureEventCount += 1
      this.#postFailureBytes += stored.record.encodedBytes
    }
  }

  #overwriteOldestPreFailure(): void {
    const oldest = this.#events.find((entry) => entry.phase === 'pre')
    if (oldest === undefined) {
      // Validated capacities guarantee an individually admissible event can fit
      // after the pre-window is emptied.
      throw new TypeError('trace pre-failure capacity cannot admit a valid event')
    }
    this.#remove(oldest)
    this.#health.increment('overwrittenCount')
  }

  #remove(stored: StoredTraceEvent<Event, Incident>): void {
    const index = this.#events.indexOf(stored)
    if (index < 0) return
    this.#events.splice(index, 1)
    this.#retainedBytes -= stored.record.encodedBytes
    if (stored.phase === 'pre') {
      this.#preFailureEventCount -= 1
      this.#preFailureBytes -= stored.record.encodedBytes
    } else {
      this.#postFailureEventCount -= 1
      this.#postFailureBytes -= stored.record.encodedBytes
    }
    const coalesced = this.#coalesced.get(stored.name)
    if (coalesced?.stored === stored) this.#coalesced.delete(stored.name)
  }

  #dropAndSealForCapacity(): void {
    this.#health.increment('droppedCount')
    this.seal('capacity_exhausted')
  }

  #notePostFailureActivity(): void {
    if (this.#state !== 'recording_post_failure') return
    const epoch = this.#timerEpoch
    this.#cancelTask(this.#silenceDeadline)
    this.#silenceDeadline = undefined
    try {
      this.#armSilenceDeadline(epoch)
    } catch {
      this.seal('post_failure_silence')
    }
  }

  #armSilenceDeadline(epoch: number): void {
    this.#silenceDeadline = this.#scheduler.schedule(
      this.#capacity.postFailureSilenceMs,
      () => {
        if (this.#timerEpoch === epoch && this.#state === 'recording_post_failure') {
          this.seal('post_failure_silence')
        }
      },
    )
  }

  #cancelPostFailureTimers(): void {
    this.#timerEpoch += 1
    this.#cancelTask(this.#postFailureDeadline)
    this.#cancelTask(this.#silenceDeadline)
    this.#postFailureDeadline = undefined
    this.#silenceDeadline = undefined
  }

  #cancelTask(task: TraceScheduledTask | undefined): void {
    try {
      task?.cancel()
    } catch {
      // Epoch fencing makes a failed timer cancellation harmless.
    }
  }

  #phase(): CapturePhase {
    return this.#state === 'recording_pre_failure' ? 'pre' : 'post'
  }

  #requireDomainEventName(value: DomainTraceEventName): DomainTraceEventName {
    if ((value as TraceEventNameV1) === 'incident_marker' ||
        !TRACE_EVENT_NAMES_V1.includes(value as TraceEventNameV1)) {
      throw new TypeError('trace event name is not part of the closed domain vocabulary')
    }
    return value
  }

  #requireEncodedBytes(value: number): number {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new RangeError('trace encoded bytes must be a positive safe integer')
    }
    return value
  }

  #readNow(fallback: number): number {
    try {
      const value = this.#clock.nowMilliseconds()
      if (Number.isSafeInteger(value) && value >= 0) {
        this.#lastNowMilliseconds = Math.max(fallback, value)
        return this.#lastNowMilliseconds
      }
    } catch {
      // A diagnostic clock cannot destabilize product work.
    }
    this.#lastNowMilliseconds = fallback
    return fallback
  }
}
