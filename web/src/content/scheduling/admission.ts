import { ContentAllocation, demandBytes, type ContentAssignment, type ContentLane, type ContentWork } from './selection'
import type { BlockReceivePermit } from './receive-window'
import type { DispatchPurpose } from './exploration'

const PRIORITIES = ['preview', 'download', 'prefetch'] as const
const WEIGHTS = { preview: 4, download: 2, prefetch: 1 } as const

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
  readonly #served = { preview: 0, download: 0, prefetch: 0 }
  #scheduled = false

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

  wake(): void {
    if (this.#scheduled) return
    this.#scheduled = true
    queueMicrotask(() => {
      this.#scheduled = false
      this.#drain()
    })
  }

  #drain(): void {
    for (const pending of this.#pending) {
      try { pending.signal.throwIfAborted(); pending.routes.assertActive() } catch (error) {
        this.#reject(pending, error)
      }
    }
    while (this.#pending.size > 0) {
      const assignment = this.#select()
      if (assignment === undefined || !this.#commit(assignment)) return
    }
  }

  #reject(pending: PendingAdmission, error: unknown): void {
    this.#pending.delete(pending)
    pending.cleanup()
    pending.reject(error)
  }

  #select(): ContentAssignment<PendingAdmission> | undefined {
    for (let round = 0; round < 2; round++) {
      for (const priority of PRIORITIES) {
        if (this.#served[priority] >= WEIGHTS[priority]) continue
        const candidates = [...this.#pending].filter(item => item.priority === priority &&
          (item.capacity?.available() ?? true)).sort(compareFrontiers)
        const assignment = this.#allocation.select(candidates, this.#lanes(), this.#now(),
          (lane, item) => item.eligible(lane))
        if (assignment !== undefined) return assignment
      }
      for (const priority of PRIORITIES) this.#served[priority] = 0
    }
    return undefined
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
      this.#served[work.priority]++
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
    const expectedMilliseconds = lane.performance.estimateCompletionMilliseconds(bytes)
    lane.inflight++
    lane.performance.begin(started, bytes)
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
