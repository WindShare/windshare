import {
  createTraceCapacityPolicy,
  type TraceCapacityPolicy,
} from './capacity'
import {
  TRACE_EVENT_NAMES_V2,
  type TraceCapturedEvent,
  type TraceCaptureSnapshot,
  type TraceCaptureState,
  type TraceEventNameV2,
  type TraceHealthCounter,
  type TraceHealthSnapshot,
  type TraceSealReason,
} from './model'
import type {
  TraceCaptureSignal,
  TraceClock,
  TraceHealthReadPort,
} from './ports'
import { TraceRetentionWindow, type TraceRetention } from './retention'

type DomainTraceEventName = Exclude<TraceEventNameV2, 'incident_marker'>

interface StoredTraceEvent<Event, Incident> {
  readonly name: TraceEventNameV2
  readonly record: TraceCapturedEvent<Event, Incident>
}

interface CoalescedEvent<Event, Incident> {
  readonly observedAtMilliseconds: number
  readonly stored: StoredTraceEvent<Event, Incident>
}

export interface BoundedTraceRecorderOptions<Event, Incident> {
  readonly captureGeneration: bigint
  readonly capacity?: TraceCapacityPolicy
  readonly clock: TraceClock
  readonly eventName: (event: Event) => DomainTraceEventName
  readonly snapshotEvent: (event: Event) => Event
  readonly eventBytes: (event: Event) => number
  readonly eventRetention?: (event: Event) => TraceRetention
  readonly snapshotIncident: (incident: Incident) => Incident
  readonly incidentMarkerBytes: (incident: Incident) => number
  readonly health?: TraceHealthAccumulator
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

/** Operation failures are evidence within a capture, never owners of its lifetime. */
export class BoundedTraceRecorder<Event, Incident> {
  readonly #captureGeneration: bigint
  readonly #capacity: TraceCapacityPolicy
  readonly #clock: TraceClock
  readonly #eventName: BoundedTraceRecorderOptions<Event, Incident>['eventName']
  readonly #snapshotEvent: BoundedTraceRecorderOptions<Event, Incident>['snapshotEvent']
  readonly #eventBytes: BoundedTraceRecorderOptions<Event, Incident>['eventBytes']
  readonly #snapshotIncident: BoundedTraceRecorderOptions<Event, Incident>['snapshotIncident']
  readonly #incidentMarkerBytes: BoundedTraceRecorderOptions<Event, Incident>['incidentMarkerBytes']
  readonly #health: TraceHealthAccumulator
  readonly #startedAtMilliseconds: number
  readonly #events: StoredTraceEvent<Event, Incident>[] = []
  readonly #retention: TraceRetentionWindow<StoredTraceEvent<Event, Incident>>
  readonly #eventRetention: BoundedTraceRecorderOptions<Event, Incident>['eventRetention']
  readonly #sampledAt = new Map<TraceEventNameV2, number>()
  readonly #coalesced = new Map<TraceEventNameV2, CoalescedEvent<Event, Incident>>()
  #state: Exclude<TraceCaptureState, 'idle'> = 'recording'
  #sealReason: TraceSealReason | undefined
  #lastNowMilliseconds: number
  #nextSequence = 1n
  #retainedBytes = 0
  #incidentMarkerCount = 0

  constructor(options: BoundedTraceRecorderOptions<Event, Incident>) {
    if (typeof options.captureGeneration !== 'bigint' || options.captureGeneration <= 0n) {
      throw new RangeError('trace capture generation must be a positive bigint')
    }
    this.#captureGeneration = options.captureGeneration
    this.#capacity = createTraceCapacityPolicy(options.capacity)
    this.#retention = new TraceRetentionWindow(this.#capacity.maxEventCount, this.#capacity.maxTotalBytes)
    this.#clock = options.clock
    this.#eventName = options.eventName
    this.#snapshotEvent = options.snapshotEvent
    this.#eventBytes = options.eventBytes
    this.#eventRetention = options.eventRetention
    this.#snapshotIncident = options.snapshotIncident
    this.#incidentMarkerBytes = options.incidentMarkerBytes
    this.#health = options.health ?? new TraceHealthAccumulator()
    this.#startedAtMilliseconds = this.#readNow(0)
    this.#lastNowMilliseconds = this.#startedAtMilliseconds
  }

  get state(): Exclude<TraceCaptureState, 'idle'> {
    return this.#state
  }

  get enabled(): boolean {
    return this.#state === 'recording'
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
    if (prepared !== undefined) this.#append(prepared)
  }

  #recordSampled(event: Event, name: DomainTraceEventName): void {
    const now = this.#readNow(this.#lastNowMilliseconds)
    const previous = this.#sampledAt.get(name)
    if (previous !== undefined && now - previous < this.#capacity.progressSampleIntervalMs) {
      this.#health.increment('sampledCount')
      return
    }
    const prepared = this.#prepareDomainEvent(event, name, now)
    if (prepared === undefined) return
    this.#append(prepared)
    this.#sampledAt.set(name, now)
  }

  #recordCoalesced(event: Event, name: DomainTraceEventName): void {
    const now = this.#readNow(this.#lastNowMilliseconds)
    const prepared = this.#prepareDomainEvent(event, name, now)
    if (prepared === undefined) return
    const previous = this.#coalesced.get(name)
    if (previous !== undefined &&
        now - previous.observedAtMilliseconds < this.#capacity.checkpointCoalesceIntervalMs) {
      // Replace at the newest chronological position; snapshots already exported
      // retain their immutable record while the live window owns only the new one.
      this.#remove(previous.stored)
      this.#health.increment('coalescedCount')
    }
    this.#append(prepared)
    this.#coalesced.set(name, Object.freeze({ observedAtMilliseconds: now, stored: prepared }))
  }

  signal(signal: TraceCaptureSignal<Incident>): boolean {
    if (!this.enabled) return false
    if (typeof signal.elapsedMs !== 'bigint' || signal.elapsedMs < 0n) {
      this.#health.increment('droppedCount')
      return false
    }
    let snapshot: Incident
    let encodedBytes: number
    try {
      snapshot = this.#snapshotIncident(signal.incident)
      encodedBytes = this.#requireEncodedBytes(this.#incidentMarkerBytes(snapshot))
    } catch {
      this.#health.increment('droppedCount')
      return false
    }
    if (encodedBytes > this.#capacity.maxEventBytes) {
      this.#health.increment('droppedCount')
      return false
    }
    if (this.#incidentMarkerCount >= this.#capacity.maxIncidentMarkers) {
      const oldest = this.#events.find(entry => entry.record.value.kind === 'incident_marker')
      if (oldest !== undefined) this.#overwrite(oldest)
    }
    const stored: StoredTraceEvent<Event, Incident> = Object.freeze({
      name: 'incident_marker' as const,
      record: Object.freeze({
        sequence: this.#nextSequence++,
        observedAtMilliseconds: this.#readNow(this.#lastNowMilliseconds),
        elapsedMs: signal.elapsedMs,
        encodedBytes,
        value: Object.freeze({
          kind: 'incident_marker' as const,
          incident: snapshot,
          eventName: 'incident_marker' as const,
        }),
      }),
    })
    this.#append(stored)
    return true
  }

  clear(): void {
    if (!this.enabled) return
    this.#events.splice(0)
    this.#retention.clear()
    this.#retainedBytes = 0
    this.#incidentMarkerCount = 0
    this.#sampledAt.clear()
    this.#coalesced.clear()
  }

  seal(reason: TraceSealReason): void {
    if (!this.enabled) return
    this.#state = 'sealed'
    this.#sealReason = reason
    this.#sampledAt.clear()
    this.#coalesced.clear()
  }

  snapshot(): TraceCaptureSnapshot<Event, Incident> {
    const events = Object.freeze(this.#events.map(entry => entry.record))
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

  #prepareDomainEvent(
    input: Event,
    name: DomainTraceEventName,
    knownNow?: number,
  ): StoredTraceEvent<Event, Incident> | undefined {
    let event: Event
    let encodedBytes: number
    try {
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
    return Object.freeze({
      name,
      record: Object.freeze({
        sequence: this.#nextSequence++,
        observedAtMilliseconds: now,
        elapsedMs: BigInt(Math.max(0, now - this.#startedAtMilliseconds)),
        encodedBytes,
        value: Object.freeze({ kind: 'event' as const, event, eventName: name }),
      }),
    })
  }

  #append(stored: StoredTraceEvent<Event, Incident>): void {
    while (this.#events.length >= this.#capacity.maxEventCount ||
           this.#retainedBytes + stored.record.encodedBytes > this.#capacity.maxTotalBytes) {
      this.#overwrite(this.#oldestReplaceable())
    }
    this.#events.push(stored)
    this.#retainedBytes += stored.record.encodedBytes
    const value = stored.record.value
    if (value.kind === 'incident_marker') this.#incidentMarkerCount += 1
    try {
      // Fault markers compete within the bounded outcome reserve, so normal
      // traffic preserves useful failures without letting failures fill memory.
      const retention = value.kind === 'incident_marker'
        ? 'outcome'
        : this.#eventRetention?.(value.event) ?? 'recent'
      this.#retention.retain(stored, stored.record.encodedBytes, retention)
    } catch {
      // Invalid retention hints cannot interfere with a valid observation.
    }
  }

  #oldestReplaceable(): StoredTraceEvent<Event, Incident> {
    let candidate = this.#events[0]
    if (candidate === undefined) {
      throw new TypeError('trace capacity cannot admit a valid event')
    }
    for (const entry of this.#events) {
      if (this.#retention.priority(entry) < this.#retention.priority(candidate)) candidate = entry
      if (this.#retention.priority(candidate) === 0) break
    }
    return candidate
  }

  #overwrite(stored: StoredTraceEvent<Event, Incident>): void {
    this.#remove(stored)
    this.#health.increment('overwrittenCount')
  }

  #remove(stored: StoredTraceEvent<Event, Incident>): void {
    const index = this.#events.indexOf(stored)
    if (index < 0) return
    this.#events.splice(index, 1)
    this.#retention.remove(stored)
    this.#retainedBytes -= stored.record.encodedBytes
    if (stored.record.value.kind === 'incident_marker') this.#incidentMarkerCount -= 1
    if (this.#coalesced.get(stored.name)?.stored === stored) this.#coalesced.delete(stored.name)
  }

  #requireDomainEventName(value: DomainTraceEventName): DomainTraceEventName {
    if ((value as TraceEventNameV2) === 'incident_marker' ||
        !TRACE_EVENT_NAMES_V2.includes(value as TraceEventNameV2)) {
      throw new TypeError('trace event name is not part of the closed domain vocabulary')
    }
    return value
  }

  #requireEncodedBytes(value: number): number {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new TypeError('trace encoded bytes must be a positive safe integer')
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
