import { ContentAllocation, demandBytes, orderedContentLanes, type ContentAssignment, type ContentLane, type ContentSelection, type ContentWork } from './selection'
import type { BlockReceivePermit } from './receive-window'
import { RESCUE_POLL_MILLISECONDS, type DispatchPurpose, type RescueReason } from './exploration'
import type { DeliveryEstimate } from './completion'
import type { SamplingOutcome } from './sampling'

const PRIORITIES = ['preview', 'download', 'prefetch'] as const
const PRIORITY_AGING_MILLISECONDS = 1000

function compareFrontiers(left: ContentWork, right: ContentWork): number {
  if (left.distance() < right.distance()) return -1
  if (left.distance() > right.distance()) return 1
  return (left.capacity?.lastServed() ?? 0) - (right.capacity?.lastServed() ?? 0)
}

export type ContentAdmission = {
  readonly lane: ContentLane
  readonly permit: BlockReceivePermit
  readonly started: number
  readonly expected: DeliveryEstimate
  close(outcome: SamplingOutcome): void
} & (
  | { readonly purpose: Exclude<DispatchPurpose, 'rescue'>; readonly rescueReason?: never }
  | { readonly purpose: 'rescue'; readonly rescueReason: RescueReason }
)

type AdmissionIntent =
  | { readonly kind: 'content'; readonly eligible: (lane: ContentLane) => boolean }
  | { readonly kind: 'rescue'; readonly eligible: (lane: ContentLane) => boolean; readonly decide: (lane: ContentLane) => RescueReason | undefined }

interface PendingAdmission extends ContentWork {
  readonly queuedAt: number
  readonly intent: AdmissionIntent
  readonly resolve: (admission: ContentAdmission) => void
  readonly reject: (reason: unknown) => void
  readonly cleanup: () => void
}

/** All attempts compete here; a physical lane never owns waiting work. */
export class ContentAdmissions {
  readonly #lanes: () => readonly ContentLane[]
  readonly #now: () => number
  readonly #pending = new Set<PendingAdmission>()
  readonly #allocation = new ContentAllocation()
  #scheduled = false
  #deadline: ReturnType<typeof setTimeout> | undefined

  constructor(lanes: () => readonly ContentLane[], now: () => number) {
    this.#lanes = lanes
    this.#now = now
  }

  request(work: ContentWork, signal: AbortSignal, eligible: (lane: ContentLane) => boolean): Promise<ContentAdmission> {
    return this.#enqueue(work, signal, { kind: 'content', eligible })
  }

  requestRescue(
    work: ContentWork, signal: AbortSignal, eligible: (lane: ContentLane) => boolean,
    decide: (lane: ContentLane) => RescueReason | undefined,
  ): Promise<ContentAdmission> {
    return this.#enqueue(work, signal, { kind: 'rescue', eligible, decide })
  }

  #enqueue(work: ContentWork, signal: AbortSignal, intent: AdmissionIntent): Promise<ContentAdmission> {
    signal.throwIfAborted()
    return new Promise((resolve, reject) => {
      const abort = () => {
        this.#pending.delete(pending)
        pending.cleanup()
        reject(signal.reason)
        this.wake()
      }
      const unsubscribe = work.routes.subscribe(() => this.wake())
      const pending: PendingAdmission = {
        demand: work.demand, routes: work.routes, signal,
        get priority() { return work.priority },
        distance: work.distance,
        ...(work.capacity === undefined ? {} : { capacity: work.capacity }),
        queuedAt: this.#now(), intent, resolve, reject,
        cleanup: () => { unsubscribe(); signal.removeEventListener('abort', abort) },
      }
      this.#pending.add(pending)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
      else this.wake()
    })
  }

  #cancelDeadline(): void {
    if (this.#deadline === undefined) return
    clearTimeout(this.#deadline)
    this.#deadline = undefined
  }

  wake(): void {
    this.#cancelDeadline()
    if (this.#pending.size === 0 || this.#scheduled) return
    this.#scheduled = true
    queueMicrotask(() => {
      this.#scheduled = false
      this.#drain()
    })
  }

  #drain(): void {
    this.#cancelDeadline()
    for (const pending of this.#pending) {
      try { pending.signal.throwIfAborted(); pending.routes.assertActive() } catch (error) {
        this.#reject(pending, error)
      }
    }
    while (this.#pending.size > 0) {
      const selection = this.#select()
      if (selection.kind === 'dispatch' && this.#commit(selection.assignment)) continue
      let at = selection.kind === 'probe-deadline' ? selection.at : Infinity
      // One owner also revisits silent rescue intents. No duplicate reservation
      // is held while waiting; fresh receipts can withdraw their justification.
      if ([...this.#pending].some(item => item.intent.kind === 'rescue')) {
        at = Math.min(at, this.#now() + RESCUE_POLL_MILLISECONDS)
      }
      if (Number.isFinite(at)) this.#deadline = setTimeout(() => {
        this.#deadline = undefined
        this.wake()
      }, Math.max(0, Math.ceil(at - this.#now())))
      return
    }
  }

  #reject(pending: PendingAdmission, error: unknown): void {
    this.#pending.delete(pending)
    pending.cleanup()
    pending.reject(error)
  }

  #select(): ContentSelection<PendingAdmission> {
    const now = this.#now()
    const candidates = [...this.#pending].filter(item => item.capacity === undefined || item.capacity.readiness() === 'ready').sort((a, b) => {
      const agedA = now - a.queuedAt >= PRIORITY_AGING_MILLISECONDS
      const agedB = now - b.queuedAt >= PRIORITY_AGING_MILLISECONDS
      if (agedA !== agedB) return agedA ? -1 : 1
      if (!agedA && a.priority !== b.priority) return PRIORITIES.indexOf(a.priority) - PRIORITIES.indexOf(b.priority)
      return compareFrontiers(a, b) || a.queuedAt - b.queuedAt
    })
    // Rescue gets the next useful credit before read-ahead can refill that lane.
    // Choosing an idle but slower alternative would turn a temporary credit wait
    // into a long, irrevocable duplicate.
    for (const work of candidates) {
      if (work.intent.kind !== 'rescue') continue
      const lane = orderedContentLanes(this.#lanes(), work).find(work.intent.eligible)
      if (lane === undefined || !lane.receive.available) continue
      const reason = work.intent.decide(lane)
      if (reason !== undefined) return { kind: 'dispatch', assignment: { work, lane, purpose: 'rescue', reason } }
    }
    return this.#allocation.select(candidates.filter(work => work.intent.kind === 'content'), this.#lanes(), now,
      (lane, work) => work.intent.kind === 'content' && work.intent.eligible(lane))
  }

  demand(): { queuedBytes: number; queuedBlocks: number; networkWaitingBytes: number; capacityWaitingBytes: number } {
    let queuedBytes = 0, queuedBlocks = 0, networkWaitingBytes = 0, capacityWaitingBytes = 0
    for (const work of this.#pending) {
      if (work.signal.aborted || work.intent.kind === 'rescue') continue
      const bytes = demandBytes(work.demand)
      queuedBytes += bytes; queuedBlocks++
      if (work.capacity?.readiness() === 'consumer') capacityWaitingBytes += bytes
      else if (this.#lanes().some(work.intent.eligible)) networkWaitingBytes += bytes
    }
    return { queuedBytes, queuedBlocks, networkWaitingBytes, capacityWaitingBytes }
  }

  #commit(assignment: ContentAssignment<PendingAdmission>): boolean {
    const { work, lane, purpose } = assignment
    let release: (() => void) | undefined
    let permit: BlockReceivePermit | undefined
    try {
      const bytes = demandBytes(work.demand)
      const expected = lane.receive.estimateCompletion(bytes)
      release = work.capacity?.acquire()
      if (work.capacity !== undefined && release === undefined) return false
      permit = lane.receive.tryAcquire(bytes, work.queuedAt)
      if (permit === undefined) { release?.(); return false }
      this.#pending.delete(work)
      work.cleanup()
      this.#allocation.started(work, purpose, this.#now())
      work.resolve(this.#begin(assignment, expected, permit, release))
    } catch (error) {
      permit?.close()
      release?.()
      this.#allocation.completed(work)
      this.#reject(work, error)
    }
    return true
  }

  #begin(
    assignment: ContentAssignment<PendingAdmission>, expected: DeliveryEstimate,
    permit: BlockReceivePermit, release: (() => void) | undefined,
  ): ContentAdmission {
    const { work, lane, purpose } = assignment
    const dispatch = assignment.purpose === 'rescue'
      ? { purpose: 'rescue' as const, rescueReason: assignment.reason } : { purpose: assignment.purpose }
    const started = this.#now()
    const bytes = demandBytes(work.demand)
    lane.performance.begin(started, bytes)
    lane.inflight++
    if (purpose !== 'rescue') lane.sampling.offered(bytes, started)
    let closed = false
    return {
      lane, ...dispatch, permit, started, expected,
      close: outcome => {
        if (closed) return
        closed = true
        lane.inflight--
        lane.performance.complete(this.#now(), bytes, outcome === 'completed')
        if (purpose !== 'rescue') lane.sampling.settled(bytes, this.#now(), outcome)
        this.#allocation.completed(work)
        permit.close()
        release?.()
        this.wake()
      },
    }
  }
}
