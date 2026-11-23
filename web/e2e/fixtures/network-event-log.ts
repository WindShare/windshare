import {
  sameLaneIdentity,
  type HotSwitchLaneObservation,
  type HotSwitchPageEvent,
} from './hot-switch-contract'

const EVENT_TIMEOUT_MILLISECONDS = 30_000
const MAXIMUM_RETAINED_EVENTS = 1_024
const PAGE_EVENT_KINDS: ReadonlySet<unknown> = new Set<HotSwitchPageEvent['kind']>([
  'attempt',
  'recovery',
  'admission-response-gated',
  'dispatch',
  'lane-admitted',
  'lane-detached',
  'relay-ineligible',
  'delivery',
  'runtime-settled',
])

type MatchingEvent<T extends HotSwitchPageEvent['kind']> = Extract<HotSwitchPageEvent, { kind: T }>

export class NetworkEventLog {
  readonly #timeout: number
  constructor(timeout = EVENT_TIMEOUT_MILLISECONDS) { this.#timeout = timeout }
  readonly #events: HotSwitchPageEvent[] = []
  readonly #waiters = new Set<(event: HotSwitchPageEvent) => void>()
  #runtimeFailure: Error | undefined

  accept(value: unknown): void {
    const event = requirePageEvent(value)
    if (this.#events.length >= MAXIMUM_RETAINED_EVENTS) {
      throw new Error('Network route event log exceeded its diagnostic bound')
    }
    this.#events.push(event)
    if (event.kind === 'runtime-settled' && event.error !== undefined) {
      this.#runtimeFailure ??= new Error(`Browser transfer failed: ${event.error}`)
    }
    for (const inspect of [...this.#waiters]) inspect(event)
  }

  waitFor<T extends HotSwitchPageEvent['kind']>(
    kind: T,
    predicate: (event: MatchingEvent<T>) => boolean,
    label: string,
  ): Promise<MatchingEvent<T>> {
    return this.waitForAny(
      (event): event is MatchingEvent<T> => event.kind === kind && predicate(event as MatchingEvent<T>),
      label,
    )
  }

  waitForAny<T extends HotSwitchPageEvent>(
    predicate: (event: HotSwitchPageEvent) => event is T,
    label: string,
  ): Promise<T> {
    const existing = this.#events.find(predicate)
    if (existing !== undefined) return Promise.resolve(existing)
    // Once the page has failed, missing milestones cannot provide more evidence.
    if (this.#runtimeFailure !== undefined) return Promise.reject(this.#runtimeFailure)
    return new Promise<T>((resolve, reject) => {
      const cleanup = () => {
        clearTimeout(timer)
        this.#waiters.delete(inspect)
      }
      const inspect = (event: HotSwitchPageEvent) => {
        if (predicate(event)) {
          cleanup()
          resolve(event)
        } else if (this.#runtimeFailure !== undefined) {
          cleanup()
          reject(this.#runtimeFailure)
        }
      }
      const timer = setTimeout(() => {
        cleanup()
        reject(new Error(`Timed out waiting for ${label}`))
      }, this.#timeout)
      this.#waiters.add(inspect)
    })
  }

  waitForLaneDispatch(
    lane: HotSwitchLaneObservation,
    afterSequence: number,
  ): Promise<MatchingEvent<'dispatch'>> {
    // A route can keep carrying traffic on its predecessor during renewal.
    // Match the admission as well as the cut boundary so that valid traffic
    // cannot be mistaken for proof that the replacement is carrying content.
    return this.waitFor(
      'dispatch',
      (event) => sameLaneIdentity(event.observation, lane) &&
        event.observation.route === lane.route &&
        event.observation.dispatchSequence > afterSequence,
      `${lane.route} lane ${lane.laneId}/${lane.laneEpoch} dispatch after ${afterSequence}`,
    )
  }

  latestDispatchSequence(): number {
    return this.#events.reduce(
      (latest, event) => event.kind === 'dispatch'
        ? Math.max(latest, event.observation.dispatchSequence)
        : latest,
      0,
    )
  }

  snapshot(): readonly HotSwitchPageEvent[] {
    return Object.freeze([...this.#events])
  }
}

function requirePageEvent(value: unknown): HotSwitchPageEvent {
  if (
    value === null || typeof value !== 'object' || Array.isArray(value) ||
    !Object.hasOwn(value, 'kind') || !PAGE_EVENT_KINDS.has((value as { kind?: unknown }).kind)
  ) throw new TypeError('Network route bridge received an invalid event')
  return value as HotSwitchPageEvent
}
