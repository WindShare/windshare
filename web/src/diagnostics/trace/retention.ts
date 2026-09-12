import type { TraceEventObservationV1 } from './model'
import type { BrowserDeliveryPayloadV1 } from './browser-delivery-payload'

export type TraceRetention = 'recent' | 'milestone' | 'outcome'
export const MAX_TRACE_PRIORITY_EVENT_COUNT = 256
export const MAX_TRACE_PRIORITY_BYTES = 262_144
const PRIORITY_BUDGET_DIVISOR = 4

interface Reservation<Event> {
  readonly events: Map<Event, number>
  bytes: number
}

/** Each protected class uses at most a quarter of the existing pre-failure window.
 * Reserving independently keeps ordinary milestones from erasing exceptional outcomes.
 */
export class TraceRetentionWindow<Event> {
  readonly #milestones: Reservation<Event> = { events: new Map(), bytes: 0 }
  readonly #outcomes: Reservation<Event> = { events: new Map(), bytes: 0 }
  readonly #countLimit: number
  readonly #byteLimit: number

  constructor(maxEventCount: number, maxBytes: number) {
    this.#countLimit = Math.min(MAX_TRACE_PRIORITY_EVENT_COUNT, Math.floor(maxEventCount / PRIORITY_BUDGET_DIVISOR))
    this.#byteLimit = Math.min(MAX_TRACE_PRIORITY_BYTES, Math.floor(maxBytes / PRIORITY_BUDGET_DIVISOR))
  }

  retain(event: Event, bytes: number, retention: TraceRetention): void {
    if (retention === 'recent') return
    const group = retention === 'outcome' ? this.#outcomes : this.#milestones
    group.events.set(event, bytes)
    group.bytes += bytes
    while (group.events.size > this.#countLimit || group.bytes > this.#byteLimit) {
      const first = group.events.entries().next().value
      if (first === undefined) break
      group.events.delete(first[0])
      group.bytes -= first[1]
    }
  }

  priority(event: Event): number {
    if (this.#outcomes.events.has(event)) return 2
    return this.#milestones.events.has(event) ? 1 : 0
  }

  remove(event: Event): void {
    for (const group of [this.#milestones, this.#outcomes]) {
      const bytes = group.events.get(event)
      if (bytes === undefined) continue
      group.events.delete(event)
      group.bytes -= bytes
    }
  }

  clear(): void {
    for (const group of [this.#milestones, this.#outcomes]) {
      group.events.clear()
      group.bytes = 0
    }
  }
}

function browserDeliveryRetention(payload: BrowserDeliveryPayloadV1): TraceRetention {
  if (payload.failure_name !== undefined || payload.checkpoint_stage === 'failed') return 'outcome'
  switch (payload.transition) {
    case 'copy-failed':
    case 'cleanup-failed':
    case 'discard-failed':
      return 'outcome'
    case 'receiving':
    case 'checkpoint':
      return 'recent'
    default:
      return 'milestone'
  }
}

export function traceEventRetention(event: TraceEventObservationV1): TraceRetention {
  switch (event.eventName) {
    case 'protocol_operation':
      switch (event.payload.transition) {
        case 'cancelled':
        case 'authenticated_failure':
        case 'late_response_discarded':
        case 'send_failed':
        case 'request_send_failed':
          return 'outcome'
        case 'admission_waiting':
        case 'admission_abandoned':
          return 'milestone'
        default:
          return 'recent'
      }
    case 'request_scheduling':
      return event.payload.transition === 'failed' ? 'outcome' : 'recent'
    case 'content_scheduling':
      return event.payload.purpose === 'rescue' ? 'milestone' : 'recent'
    case 'peer_attempt':
      if (event.payload.stage === 'failed' || event.payload.stage === 'admitted') return 'outcome'
      return event.payload.stage === 'provider_fact' ? 'recent' : 'milestone'
    case 'output_write':
    case 'output_reservation':
    case 'checkpoint':
    case 'transfer_progress':
    case 'direct_zip_coordination':
      return 'recent'
    case 'receive_transition':
      switch (event.payload.transition) {
        case 'materialization_completed':
        case 'materialization_failed':
        case 'download_connectivity':
          return 'outcome'
        case 'directory_admitted':
          return 'recent'
        default:
          return 'milestone'
      }
    case 'browser_delivery':
      return browserDeliveryRetention(event.payload)
    case 'direct_zip_member_rollback':
      return 'outcome'
    default:
      return 'milestone'
  }
}
