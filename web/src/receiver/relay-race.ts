export const MAXIMUM_RECEIVER_RELAYS = 8

export class RelayEndpointFailure extends Error {
  readonly relayBase: string
  constructor(relayBase: string, cause: unknown) {
    super(`Relay endpoint ${relayBase} failed`, { cause })
    this.name = 'RelayEndpointFailure'
    this.relayBase = relayBase
  }
}

export function receiverRelayBases(values: readonly string[]): readonly string[] {
  const unique = [...new Set(values.map((value) => value.trim()))]
  if (unique.length === 0 || unique.length > MAXIMUM_RECEIVER_RELAYS || unique.includes('')) {
    throw new RangeError(`A receiver needs between 1 and ${MAXIMUM_RECEIVER_RELAYS} relay endpoints`)
  }
  return Object.freeze(unique)
}

/** The first usable authenticated path wins; slow losers cannot retain connection authority. */
export async function firstUsableRelay<T>(
  relayBases: readonly string[],
  signal: AbortSignal,
  connect: (relayBase: string, signal: AbortSignal) => Promise<T>,
  close: (value: T) => Promise<void>,
  fatal?: (error: unknown) => boolean,
): Promise<T> {
  signal.throwIfAborted()
  const controllers = relayBases.map(() => new AbortController())
  const abort = () => controllers.forEach((controller) => controller.abort(signal.reason))
  signal.addEventListener('abort', abort, { once: true })
  let claimed = false
  let fail!: (error: unknown) => void
  const terminal = new Promise<never>((_resolve, reject) => { fail = reject })
  try {
    const result = await Promise.race([terminal, Promise.any(relayBases.map(async (relayBase, index) => {
      const controller = controllers[index]!
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
    }))])
    return result
  } catch (error) {
    signal.throwIfAborted()
    if (error instanceof AggregateError && error.errors.length === 1) throw error.errors[0]
    throw error
  } finally {
    signal.removeEventListener('abort', abort)
  }
}
