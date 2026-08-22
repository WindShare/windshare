import type { OutputTraceSource } from '../../diagnostics/trace'
import { emitOutputTrace, outputTraceEvent } from '../../diagnostics/trace'
import type {
  DirectZipDiagnosticClock,
  DirectZipDiagnosticMilestoneInput,
  DirectZipLocalDiagnosticRecord,
} from './model'
import {
  DIRECT_ZIP_LOCAL_DIAGNOSTIC_CAPACITY,
  projectDirectZipDiagnosticV1,
  requireDirectZipDiagnosticCapacity,
  snapshotDirectZipLocalDiagnostic,
} from './projection'

export interface DirectZipDiagnosticsObserver {
  observe(input: DirectZipDiagnosticMilestoneInput): void
}

/**
 * Recording is fail-closed from the authority path's perspective: invalid facts,
 * clocks, allocation failures, and observer exceptions can only drop diagnostics.
 */
export class BoundedDirectZipDiagnosticHistory implements DirectZipDiagnosticsObserver {
  readonly #capacity: number
  readonly #clock: DirectZipDiagnosticClock
  readonly #trace: OutputTraceSource | undefined
  readonly #records: DirectZipLocalDiagnosticRecord[] = []
  #droppedCount = 0n

  constructor(options: {
    readonly clock: DirectZipDiagnosticClock
    readonly trace?: OutputTraceSource
    readonly capacity?: number
  }) {
    this.#capacity = requireDirectZipDiagnosticCapacity(
      options.capacity ?? DIRECT_ZIP_LOCAL_DIAGNOSTIC_CAPACITY,
    )
    this.#clock = options.clock
    this.#trace = options.trace
  }

  observe(input: DirectZipDiagnosticMilestoneInput): void {
    try {
      const record = snapshotDirectZipLocalDiagnostic(input, this.#clock.nowMilliseconds())
      this.#records.push(record)
      if (this.#records.length > this.#capacity) {
        this.#records.shift()
        this.#droppedCount += 1n
      }
      emitOutputTrace(this.#trace, () => outputTraceEvent(
        'direct_zip_milestone',
        projectDirectZipDiagnosticV1(record),
      ))
    } catch {
      this.#droppedCount += 1n
    }
  }

  snapshot(): readonly DirectZipLocalDiagnosticRecord[] {
    return Object.freeze([...this.#records])
  }

  droppedCount(): bigint {
    return this.#droppedCount
  }
}

export function observeDirectZipDiagnostic(
  observer: DirectZipDiagnosticsObserver | undefined,
  createInput: () => DirectZipDiagnosticMilestoneInput,
): void {
  if (observer === undefined) return
  try {
    observer.observe(createInput())
  } catch {
    // Diagnostics are passive and never participate in target or journal authority.
  }
}
