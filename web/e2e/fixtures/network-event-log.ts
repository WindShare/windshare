import type { HotSwitchPageEvent } from './hot-switch-contract'

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

interface EventWaiter {
  readonly kind: HotSwitchPageEvent['kind']
  readonly predicate: (event: HotSwitchPageEvent) => boolean
  readonly resolve: (event: HotSwitchPageEvent) => void
  readonly timer: ReturnType<typeof setTimeout>
}

export class NetworkEventLog {
  readonly #events: HotSwitchPageEvent[] = []
  readonly #waiters = new Set<EventWaiter>()

  accept(value: unknown): void {
    const event = requirePageEvent(value)
    if (this.#events.length >= MAXIMUM_RETAINED_EVENTS) {
      throw new Error('Network route event log exceeded its diagnostic bound')
    }
    this.#events.push(event)
    for (const waiter of [...this.#waiters]) {
      if (event.kind !== waiter.kind || !waiter.predicate(event)) continue
      clearTimeout(waiter.timer)
      this.#waiters.delete(waiter)
      waiter.resolve(event)
    }
  }

  waitFor<T extends HotSwitchPageEvent['kind']>(
    kind: T,
    predicate: (event: MatchingEvent<T>) => boolean,
    label: string,
  ): Promise<MatchingEvent<T>> {
    const existing = this.#events.find(
      (event): event is MatchingEvent<T> => event.kind === kind && predicate(event as MatchingEvent<T>),
    )
    if (existing !== undefined) return Promise.resolve(existing)
    return new Promise<MatchingEvent<T>>((resolve, reject) => {
      const waiter: EventWaiter = {
        kind,
        predicate: (event) => predicate(event as MatchingEvent<T>),
        resolve: (event) => resolve(event as MatchingEvent<T>),
        timer: setTimeout(() => {
          this.#waiters.delete(waiter)
          reject(new Error(`Timed out waiting for ${label}`))
        }, EVENT_TIMEOUT_MILLISECONDS),
      }
      this.#waiters.add(waiter)
    })
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
