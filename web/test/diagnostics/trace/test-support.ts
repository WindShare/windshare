import {
  createTraceCapacityPolicy,
  type TraceCapacityPolicy,
} from '../../../src/diagnostics/trace/capacity'
import type { TraceEventNameV1 } from '../../../src/diagnostics/trace/model'
import {
  BoundedTraceRecorder,
  TraceHealthAccumulator,
} from '../../../src/diagnostics/trace/recorder'
import type {
  TraceClock,
  TraceScheduledTask,
  TraceScheduler,
} from '../../../src/diagnostics/trace/ports'

type TestEventName = Extract<
  TraceEventNameV1,
  'transfer_progress' | 'checkpoint' | 'cleanup'
>

export interface TestEvent {
  readonly name: TestEventName
  readonly ordinal: number
  readonly bytes: number
}

export interface TestScope {
  readonly kind: 'receive' | 'preview'
  readonly sequence: bigint
}

export interface TestIncident {
  readonly sequence: bigint
  readonly scope: TestScope
}

interface FakeTask {
  readonly due: number
  readonly callback: () => void
  cancelled: boolean
}

export class FakeTraceTime implements TraceClock, TraceScheduler {
  #now = 1_000
  readonly tasks: FakeTask[] = []

  nowMilliseconds(): number {
    return this.#now
  }

  schedule(delayMilliseconds: number, task: () => void): TraceScheduledTask {
    const scheduled: FakeTask = {
      due: this.#now + delayMilliseconds,
      callback: task,
      cancelled: false,
    }
    this.tasks.push(scheduled)
    return Object.freeze({ cancel: () => { scheduled.cancelled = true } })
  }

  advance(milliseconds: number): void {
    const target = this.#now + milliseconds
    for (;;) {
      const pending = this.tasks
        .filter((task) => !task.cancelled && task.due <= target)
        .sort((left, right) => left.due - right.due)[0]
      if (pending === undefined) break
      pending.cancelled = true
      this.#now = pending.due
      pending.callback()
    }
    this.#now = target
  }

  runEvenIfCancelled(index: number): void {
    const task = this.tasks[index]
    if (task === undefined) throw new RangeError('fake trace task does not exist')
    task.callback()
  }
}

export function testTraceCapacity(
  overrides: Partial<TraceCapacityPolicy> = {},
): TraceCapacityPolicy {
  return createTraceCapacityPolicy({
    captureExpiryMs: 100,
    maxEventCount: 8,
    maxEventBytes: 4,
    maxTotalBytes: 16,
    maxPreFailureEventCount: 4,
    maxPreFailureBytes: 8,
    maxPostFailureEventCount: 4,
    maxPostFailureBytes: 8,
    maxIncidentMarkers: 2,
    maxPostFailureMs: 20,
    postFailureSilenceMs: 5,
    progressSampleIntervalMs: 10,
    checkpointCoalesceIntervalMs: 10,
    ...overrides,
  })
}

export function testEvent(
  ordinal: number,
  options: Readonly<{ name?: TestEventName; bytes?: number }> = {},
): TestEvent {
  return Object.freeze({
    name: options.name ?? 'cleanup',
    ordinal,
    bytes: options.bytes ?? 1,
  })
}

export function testIncident(
  sequence: bigint,
  scope: TestScope = Object.freeze({ kind: 'receive', sequence: 1n }),
): TestIncident {
  return Object.freeze({ sequence, scope })
}

export function makeTestRecorder(
  time: FakeTraceTime,
  capacity: TraceCapacityPolicy = testTraceCapacity(),
  health = new TraceHealthAccumulator(),
): BoundedTraceRecorder<TestEvent, TestIncident, TestScope> {
  return new BoundedTraceRecorder({
    captureGeneration: 1n,
    capacity,
    clock: time,
    scheduler: time,
    eventName: (event) => event.name,
    snapshotEvent: (event) => Object.freeze({ ...event }),
    eventBytes: (event) => event.bytes,
    snapshotIncident: (incident) => Object.freeze({
      sequence: incident.sequence,
      scope: Object.freeze({ ...incident.scope }),
    }),
    incidentMarkerBytes: () => 1,
    incidentScope: (incident) => incident.scope,
    sameScope: (left, right) => left.kind === right.kind && left.sequence === right.sequence,
    health,
  })
}

export function retainedOrdinals(
  recorder: BoundedTraceRecorder<TestEvent, TestIncident, TestScope>,
): number[] {
  return recorder.snapshot().events.flatMap((record) =>
    record.value.kind === 'event' ? [record.value.event.ordinal] : [])
}
