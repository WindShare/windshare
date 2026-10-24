import { ContentAllocation, demandBytes, type ContentAssignment, type ContentLane, type ContentSelection, type ContentWork } from './selection'
import type { BlockReceivePermit } from './receive-window'
import type { DispatchPurpose } from './exploration'

const PRIORITIES = ['preview', 'download', 'prefetch'] as const
const PRIORITY_AGING_MILLISECONDS = 1000

function compareFrontiers(left: ContentWork, right: ContentWork): number {
  if (left.distance() < right.distance()) return -1
  if (left.distance() > right.distance()) return 1
  return (left.capacity?.lastServed() ?? 0) - (right.capacity?.lastServed() ?? 0)
}

export interface ContentAdmission {
  readonly lane: ContentLane
  readonly purpose: DispatchPurpose
  readonly permit: BlockReceivePermit
  readonly started: number
  readonly expectedMilliseconds: number
  close(successful: boolean): void
}

interface PendingAdmission extends ContentWork {
  readonly queuedAt: number
  readonly eligible: (lane: ContentLane) => boolean
  readonly purpose: DispatchPurpose
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
  #probeWake: ReturnType<typeof setTimeout> | undefined

  constructor(lanes: () => readonly ContentLane[], now: () => number) {
    this.#lanes = lanes
    this.#now = now
  }

  request(
    work: ContentWork, signal: AbortSignal, eligible: (lane: ContentLane) => boolean,
    purpose: DispatchPurpose = 'content',
  ): Promise<ContentAdmission> {
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
        queuedAt: this.#now(), eligible, purpose, resolve, reject,
        cleanup: () => { unsubscribe(); signal.removeEventListener('abort', abort) },
      }
      this.#pending.add(pending)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
      else this.wake()
    })
  }

  #cancelProbeWake(): void {
    if (this.#probeWake === undefined) return
    clearTimeout(this.#probeWake)
    this.#probeWake = undefined
  }

  wake(): void {
    this.#cancelProbeWake()
    if (this.#pending.size === 0 || this.#scheduled) return
    this.#scheduled = true
    queueMicrotask(() => {
      this.#scheduled = false
      this.#drain()
    })
  }

  #drain(): void {
    this.#cancelProbeWake()
    for (const pending of this.#pending) {
      try { pending.signal.throwIfAborted(); pending.routes.assertActive() } catch (error) {
        this.#reject(pending, error)
      }
    }
    while (this.#pending.size > 0) {
      const selection = this.#select()
      if (selection.kind === 'probe-deadline') {
        this.#probeWake = setTimeout(() => {
          this.#probeWake = undefined
          this.wake()
        }, Math.max(0, Math.ceil(selection.at - this.#now())))
      }
      if (selection.kind !== 'dispatch' || !this.#commit(selection.assignment)) return
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
    return this.#allocation.select(candidates, this.#lanes(), now, (lane, item) => item.eligible(lane))
  }

  demand(): { queuedBytes: number; queuedBlocks: number; networkWaitingBytes: number; capacityWaitingBytes: number } {
    let queuedBytes = 0, queuedBlocks = 0, networkWaitingBytes = 0, capacityWaitingBytes = 0
    for (const work of this.#pending) {
      if (work.signal.aborted || work.purpose === 'rescue') continue
      const bytes = demandBytes(work.demand)
      queuedBytes += bytes; queuedBlocks++
      if (work.capacity?.readiness() === 'consumer') capacityWaitingBytes += bytes
      else if (this.#lanes().some(work.eligible)) networkWaitingBytes += bytes
    }
    return { queuedBytes, queuedBlocks, networkWaitingBytes, capacityWaitingBytes }
  }

  #commit(assignment: ContentAssignment<PendingAdmission>): boolean {
    const { work, lane } = assignment
    let release: (() => void) | undefined
    try {
      release = work.capacity?.acquire()
      if (work.capacity !== undefined && release === undefined) return false
      const permit = lane.receive.tryAcquire(demandBytes(work.demand), work.queuedAt)
      if (permit === undefined) { release?.(); return false }
      const purpose = work.purpose === 'rescue' ? 'rescue' : assignment.purpose
      this.#pending.delete(work)
      work.cleanup()
      this.#allocation.started(work, purpose, this.#now())
      work.resolve(this.#begin(work, lane, purpose, permit, release))
    } catch (error) {
      release?.()
      this.#reject(work, error)
    }
    return true
  }

  #begin(
    work: PendingAdmission, lane: ContentLane, purpose: DispatchPurpose,
    permit: BlockReceivePermit, release: (() => void) | undefined,
  ): ContentAdmission {
    const started = this.#now()
    const bytes = demandBytes(work.demand)
    const expectedMilliseconds = lane.performance.begin(started, bytes)
    lane.inflight++
    let closed = false
    return {
      lane, purpose, permit, started, expectedMilliseconds,
      close: successful => {
        if (closed) return
        closed = true
        lane.inflight--
        lane.performance.complete(this.#now(), bytes, successful)
        this.#allocation.completed(work)
        permit.close()
        release?.()
        this.wake()
      },
    }
  }
}
