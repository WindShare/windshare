import { relayCandidates, type RelayCandidateSource } from './relay-candidates'
export { MAXIMUM_RECEIVER_RELAYS, receiverRelayBases } from './relay-candidates'

export class RelayEndpointFailure extends Error {
  readonly relayBase: string
  constructor(relayBase: string, cause: unknown) {
    super(`Relay endpoint ${relayBase} failed`, { cause })
    this.name = 'RelayEndpointFailure'
    this.relayBase = relayBase
  }
}

/** The first usable authenticated path wins; slow losers cannot retain connection authority. */
export async function firstUsableRelay<T>(
  source: RelayCandidateSource,
  signal: AbortSignal,
  connect: (relayBase: string, signal: AbortSignal) => Promise<T>,
  close: (value: T) => Promise<void>,
  fatal?: (error: unknown) => boolean,
): Promise<T> {
  signal.throwIfAborted()
  const candidates = relayCandidates(source), initial = candidates.bases
  const controllers: AbortController[] = []
  let claimed = false
  let fail!: (error: unknown) => void
  const terminal = new Promise<never>((_resolve, reject) => { fail = reject })
  const abort = () => { controllers.forEach(controller => controller.abort(signal.reason)); fail(signal.reason) }
  signal.addEventListener('abort', abort, { once: true })
  try {
    const attempt = async (relayBase: string): Promise<T> => {
      signal.throwIfAborted()
      if (claimed) throw new DOMException('Another relay connected first', 'AbortError')
      const controller = new AbortController()
      controllers.push(controller)
      let value: T
      try {
        value = await connect(relayBase, controller.signal)
      } catch (cause) {
        if (!claimed && !signal.aborted && fatal?.(cause)) {
          claimed = true
          controllers.forEach(other => other.abort(cause))
          fail(new RelayEndpointFailure(relayBase, cause))
        }
        throw new RelayEndpointFailure(relayBase, cause)
      }
      if (claimed || signal.aborted) {
        await close(value).catch(() => undefined)
        throw signal.reason ?? new DOMException('Another relay connected first', 'AbortError')
      }
      claimed = true
      controllers.forEach((other) => {
        if (other !== controller) other.abort(new DOMException('Another relay connected first', 'AbortError'))
      })
      return value
    }
    const attempts = initial.map(attempt)
    attempts.push(candidates.all().then(bases => {
      signal.throwIfAborted()
      return Promise.any(bases.filter(base => !initial.includes(base)).map(attempt))
    }))
    const result = await Promise.race([terminal, Promise.any(attempts)])
    return result
  } catch (error) {
    signal.throwIfAborted()
    if (error instanceof AggregateError) {
      const flatten = (cause: unknown): unknown[] => cause instanceof AggregateError ? (cause.errors as unknown[]).flatMap(flatten) : [cause]
      const failures = flatten(error)
      if (failures.length === 1) throw failures[0]
      throw new AggregateError(failures, 'No relay endpoint admitted this share', { cause: error })
    }
    throw error
  } finally {
    signal.removeEventListener('abort', abort)
  }
}
