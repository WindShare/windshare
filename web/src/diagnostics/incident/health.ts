import type { TraceHealthSnapshot } from '../trace/model'
import type { TraceHealthReadPort } from '../trace/ports'

export interface IncidentDiagnosticsHealthSnapshot {
  readonly factOverflowCount: bigint
  readonly incidentHistoryEvictionCount: bigint
  readonly consoleSuppressionCount: bigint
  readonly lateLinkEvictionCount: bigint
  readonly traceDroppedCount: bigint
  readonly traceOverwrittenCount: bigint
  readonly traceSampledCount: bigint
  readonly traceCoalescedCount: bigint
}

export interface IncidentHealthReadPort {
  incidentHealthSnapshot(): IncidentDiagnosticsHealthSnapshot
}

const EMPTY_TRACE_HEALTH: TraceHealthSnapshot = Object.freeze({
  droppedCount: 0n,
  overwrittenCount: 0n,
  sampledCount: 0n,
  coalescedCount: 0n,
})

export class IncidentDiagnosticsHealth
implements IncidentHealthReadPort {
  readonly #traceHealth: TraceHealthReadPort | undefined
  #factOverflowCount = 0n
  #incidentHistoryEvictionCount = 0n
  #consoleSuppressionCount = 0n
  #lateLinkEvictionCount = 0n
  #lastTraceHealth = EMPTY_TRACE_HEALTH

  constructor(traceHealth?: TraceHealthReadPort) {
    this.#traceHealth = traceHealth
  }

  recordFactOverflow(count: bigint): void {
    this.#factOverflowCount += requireCount(count, 'fact overflow')
  }

  recordHistoryEviction(count: bigint = 1n): void {
    this.#incidentHistoryEvictionCount += requireCount(
      count,
      'incident history eviction',
    )
  }

  recordConsoleSuppression(count: bigint = 1n): void {
    this.#consoleSuppressionCount += requireCount(count, 'console suppression')
  }

  recordLateLinkEviction(count: bigint = 1n): void {
    this.#lateLinkEvictionCount += requireCount(count, 'late-link eviction')
  }

  incidentHealthSnapshot(): IncidentDiagnosticsHealthSnapshot {
    const trace = this.#readTraceHealth()
    return Object.freeze({
      factOverflowCount: this.#factOverflowCount,
      incidentHistoryEvictionCount: this.#incidentHistoryEvictionCount,
      consoleSuppressionCount: this.#consoleSuppressionCount,
      lateLinkEvictionCount: this.#lateLinkEvictionCount,
      traceDroppedCount: trace.droppedCount,
      traceOverwrittenCount: trace.overwrittenCount,
      traceSampledCount: trace.sampledCount,
      traceCoalescedCount: trace.coalescedCount,
    })
  }

  #readTraceHealth(): TraceHealthSnapshot {
    if (this.#traceHealth === undefined) return this.#lastTraceHealth
    try {
      const candidate = this.#traceHealth.traceHealthSnapshot()
      requireTraceHealth(candidate)
      this.#lastTraceHealth = Object.freeze({ ...candidate })
    } catch {
      // A fallible trace reader cannot suppress default-on incident reporting.
    }
    return this.#lastTraceHealth
  }
}

function requireTraceHealth(
  snapshot: TraceHealthSnapshot,
): void {
  requireCount(snapshot.droppedCount, 'trace dropped')
  requireCount(snapshot.overwrittenCount, 'trace overwritten')
  requireCount(snapshot.sampledCount, 'trace sampled')
  requireCount(snapshot.coalescedCount, 'trace coalesced')
}

function requireCount(value: bigint, field: string): bigint {
  if (typeof value !== 'bigint' || value < 0n) {
    throw new RangeError(`${field} count must be a non-negative bigint`)
  }
  return value
}
