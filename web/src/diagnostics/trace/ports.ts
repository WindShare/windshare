import type { TraceHealthSnapshot } from './model'

export type TraceObserver<Event> = (event: Event) => void

/**
 * Long-lived producers retain this source, not an observer snapshot, so tracing
 * can be revoked without keeping an always-present payload-building callback.
 */
export interface DomainTraceSource<Event> {
  readonly current: TraceObserver<Event> | undefined
}

export interface TraceClock {
  nowMilliseconds(): number
}

export interface TraceScheduledTask {
  cancel(): void
}

export interface TraceScheduler {
  schedule(delayMilliseconds: number, task: () => void): TraceScheduledTask
}

export interface TraceHealthReadPort {
  traceHealthSnapshot(): TraceHealthSnapshot
}

export type TraceCaptureSignal<Incident, Scope> =
  | Readonly<{ kind: 'incident_sealed'; incident: Incident; elapsedMs: bigint }>
  | Readonly<{ kind: 'scope_terminal'; scope: Scope; elapsedMs: bigint }>

export const SYSTEM_TRACE_CLOCK: TraceClock = Object.freeze({
  nowMilliseconds: () => Date.now(),
})

export const SYSTEM_TRACE_SCHEDULER: TraceScheduler = Object.freeze({
  schedule: (delayMilliseconds: number, task: () => void) => {
    const timer = globalThis.setTimeout(task, delayMilliseconds)
    return Object.freeze({ cancel: () => globalThis.clearTimeout(timer) })
  },
})
