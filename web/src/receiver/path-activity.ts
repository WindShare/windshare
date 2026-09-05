import type { V2ContentLaneAdmissionObservation } from '../connectivity/v2-receiver-policy'
import type { V2BlockRouteObservation } from '../content/v2-lane-set'

export const RECENT_CONTENT_WINDOW_MILLISECONDS = 5_000
export interface ReceiverPathActivitySnapshot {
  readonly directConnected: boolean
  readonly content: 'idle' | 'direct' | 'relay' | 'parallel'
}
export const EMPTY_RECEIVER_PATH_ACTIVITY: ReceiverPathActivitySnapshot = Object.freeze({
  directConnected: false, content: 'idle',
})

/** Connection evidence and useful content activity have separate clocks and authority. */
export class ReceiverPathActivity {
  readonly #now: () => number
  readonly #listeners = new Set<(snapshot: ReceiverPathActivitySnapshot) => void>()
  readonly #directLanes = new Map<number, number>()
  #generation = 0
  #lastDirect = -Infinity
  #lastRelay = -Infinity
  #timer: ReturnType<typeof setTimeout> | undefined
  #snapshot = EMPTY_RECEIVER_PATH_ACTIVITY
  #closed = false

  constructor(now: () => number = () => performance.now()) { this.#now = now }

  generationInstalled(generation: number): void {
    this.#generation = generation
    this.#directLanes.clear()
    this.#refresh()
  }

  generationRetired(generation: number): void {
    if (generation !== this.#generation) return
    this.#generation = 0
    this.#directLanes.clear()
    this.#refresh()
  }

  admitted(generation: number, lane: V2ContentLaneAdmissionObservation): void {
    if (this.#closed || generation !== this.#generation || lane.route !== 'direct') return
    this.#directLanes.set(lane.laneId, lane.laneEpoch)
    this.#refresh()
  }

  detached(generation: number, lane: V2ContentLaneAdmissionObservation): void {
    if (generation !== this.#generation || this.#directLanes.get(lane.laneId) !== lane.laneEpoch) return
    this.#directLanes.delete(lane.laneId)
    this.#refresh()
  }

  fetched(generation: number, fact: V2BlockRouteObservation): void {
    if (this.#closed || generation !== this.#generation || fact.usefulBytes <= 0) return
    if (fact.route === 'direct') this.#lastDirect = this.#now()
    else this.#lastRelay = this.#now()
    this.#refresh()
  }

  subscribe(listener: (snapshot: ReceiverPathActivitySnapshot) => void): () => void {
    if (this.#closed) return () => undefined
    this.#listeners.add(listener)
    listener(this.#snapshot)
    return () => this.#listeners.delete(listener)
  }

  close(): void {
    this.#closed = true
    if (this.#timer !== undefined) clearTimeout(this.#timer)
    this.#directLanes.clear()
    this.#lastDirect = -Infinity
    this.#lastRelay = -Infinity
    this.#publish(EMPTY_RECEIVER_PATH_ACTIVITY)
    this.#listeners.clear()
  }

  #refresh(): void {
    if (this.#closed) return
    if (this.#timer !== undefined) clearTimeout(this.#timer)
    const now = this.#now()
    const direct = now - this.#lastDirect < RECENT_CONTENT_WINDOW_MILLISECONDS
    const relay = now - this.#lastRelay < RECENT_CONTENT_WINDOW_MILLISECONDS
    let content: ReceiverPathActivitySnapshot['content'] = 'idle'
    if (direct && relay) content = 'parallel'
    else if (direct) content = 'direct'
    else if (relay) content = 'relay'
    this.#publish(Object.freeze({ directConnected: this.#directLanes.size > 0, content }))
    const expiry = Math.min(...[this.#lastDirect, this.#lastRelay]
      .map((time) => time + RECENT_CONTENT_WINDOW_MILLISECONDS).filter((time) => time > now))
    this.#timer = Number.isFinite(expiry) ? setTimeout(() => this.#refresh(), expiry - now) : undefined
  }

  #publish(snapshot: ReceiverPathActivitySnapshot): void {
    if (snapshot.directConnected === this.#snapshot.directConnected && snapshot.content === this.#snapshot.content) return
    this.#snapshot = snapshot
    for (const listener of this.#listeners) {
      try { listener(snapshot) } catch { /* A presentation subscriber cannot control transport work. */ }
    }
  }
}
