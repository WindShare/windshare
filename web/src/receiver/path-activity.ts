import type { V2ContentLaneAdmissionObservation } from '../connectivity/v2-receiver-policy'
import type { V2BlockRouteObservation } from '../content/v2-lane-set'

export const RECENT_CONTENT_WINDOW_MILLISECONDS = 5_000

export interface ReceiverContentLaneSnapshot extends V2ContentLaneAdmissionObservation {
  readonly recentContent: boolean
}

export interface ReceiverPathActivitySnapshot {
  readonly lanes: readonly ReceiverContentLaneSnapshot[]
}

export const EMPTY_RECEIVER_PATH_ACTIVITY: ReceiverPathActivitySnapshot = Object.freeze({
  lanes: Object.freeze([]),
})

interface LaneActivity {
  readonly admission: V2ContentLaneAdmissionObservation
  lastContent: number
}

/** Only admitted lanes attest connectivity; authenticated blocks attest recent activity. */
export class ReceiverPathActivity {
  readonly #now: () => number
  readonly #listeners = new Set<(snapshot: ReceiverPathActivitySnapshot) => void>()
  readonly #lanes = new Map<number, LaneActivity>()
  #generation: number | null = null
  #timer: ReturnType<typeof setTimeout> | undefined
  #snapshot = EMPTY_RECEIVER_PATH_ACTIVITY
  #closed = false

  constructor(now: () => number = () => performance.now()) { this.#now = now }

  generationInstalled(generation: number): void {
    if (this.#closed || generation === this.#generation) return
    this.#generation = generation
    this.#lanes.clear()
    this.#refresh()
  }

  generationRetired(generation: number): void {
    if (generation !== this.#generation) return
    this.#generation = null
    this.#lanes.clear()
    this.#refresh()
  }

  admitted(generation: number, lane: V2ContentLaneAdmissionObservation): void {
    if (this.#closed || generation !== this.#generation) return
    const previous = this.#lanes.get(lane.laneId)
    if (previous !== undefined && matches(previous.admission, lane)) return
    this.#lanes.set(lane.laneId, {
      admission: Object.freeze({ laneId: lane.laneId, laneEpoch: lane.laneEpoch, route: lane.route }),
      lastContent: -Infinity,
    })
    this.#refresh()
  }

  detached(generation: number, lane: V2ContentLaneAdmissionObservation): void {
    if (this.#closed || generation !== this.#generation) return
    const current = this.#lanes.get(lane.laneId)
    if (current === undefined || !matches(current.admission, lane)) return
    this.#lanes.delete(lane.laneId)
    this.#refresh()
  }

  fetched(generation: number, fact: V2BlockRouteObservation): void {
    if (this.#closed || generation !== this.#generation || fact.usefulBytes <= 0) return
    const current = this.#lanes.get(fact.laneId)
    // An old block can finish after replacement without proving the new lane is carrying data.
    if (current === undefined || !matches(current.admission, fact)) return
    current.lastContent = this.#now()
    this.#refresh()
  }

  subscribe(listener: (snapshot: ReceiverPathActivitySnapshot) => void): () => void {
    if (this.#closed) return () => undefined
    this.#listeners.add(listener)
    try { listener(this.#snapshot) } catch { /* Presentation cannot control transport work. */ }
    return () => this.#listeners.delete(listener)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#generation = null
    if (this.#timer !== undefined) clearTimeout(this.#timer)
    this.#lanes.clear()
    this.#publish(EMPTY_RECEIVER_PATH_ACTIVITY)
    this.#listeners.clear()
  }

  #refresh(): void {
    if (this.#closed) return
    if (this.#timer !== undefined) clearTimeout(this.#timer)
    const now = this.#now()
    let expiry = Infinity
    const lanes = [...this.#lanes.values()].sort((left, right) => left.admission.laneId - right.admission.laneId)
      .map(({ admission, lastContent }) => {
        const until = lastContent + RECENT_CONTENT_WINDOW_MILLISECONDS
        const recentContent = until > now
        if (recentContent) expiry = Math.min(expiry, until)
        return Object.freeze({ ...admission, recentContent })
      })
    this.#publish(Object.freeze({ lanes: Object.freeze(lanes) }))
    this.#timer = Number.isFinite(expiry) ? setTimeout(() => this.#refresh(), expiry - now) : undefined
  }

  #publish(snapshot: ReceiverPathActivitySnapshot): void {
    const previous = this.#snapshot.lanes
    if (snapshot.lanes.length === previous.length && snapshot.lanes.every((lane, index) =>
      matches(lane, previous[index]!) && lane.recentContent === previous[index]!.recentContent)) return
    this.#snapshot = snapshot
    for (const listener of this.#listeners) {
      try { listener(snapshot) } catch { /* A presentation subscriber cannot control transport work. */ }
    }
  }
}

function matches(left: V2ContentLaneAdmissionObservation, right: V2ContentLaneAdmissionObservation): boolean {
  return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch && left.route === right.route
}
