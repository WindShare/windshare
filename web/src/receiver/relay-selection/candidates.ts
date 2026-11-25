export const MAXIMUM_RECEIVER_RELAYS = 8
export type RelayCandidateSource = readonly string[] | RelayCandidates
export type RelayExpansionStatus = 'complete' | 'incomplete'
export interface RelayExpansionOptions {
  readonly explicit?: boolean
}
export type RelayExpansionRunner = (
  emit: (bases: readonly string[]) => void,
  options: RelayExpansionOptions,
) => Promise<RelayExpansionStatus | void>

export function receiverRelayBases(values: readonly string[]): readonly string[] {
  const unique = [...new Set(values.map(value => value.trim()))]
  if (unique.length === 0 || unique.length > MAXIMUM_RECEIVER_RELAYS || unique.includes('')) throw new RangeError(`A receiver needs between 1 and ${MAXIMUM_RECEIVER_RELAYS} relay endpoints`)
  return Object.freeze(unique)
}

/**
 * Owns candidate relay discovery across retries and protocol generations.
 * Transient expansion failures stay retryable instead of locking the share.
 */
export class RelayCandidates {
  #bases: readonly string[]
  readonly #listeners = new Set<(bases: readonly string[]) => void>()
  readonly #runner: RelayExpansionRunner | undefined
  #current: Promise<unknown>
  #expanding: boolean
  #complete: boolean
  #expansionFailure: unknown

  constructor(
    initial: readonly string[],
    expansion?: Promise<readonly string[]> | RelayExpansionRunner,
  ) {
    this.#bases = initial.length === 0 && expansion ? [] : receiverRelayBases(initial)
    if (typeof expansion === 'function') {
      this.#runner = expansion
      this.#expanding = false
      this.#complete = false
      this.#current = Promise.resolve(undefined)
      // Opening a share is an explicit user action; subsequent automatic retries
      // default to non-explicit so directory backoff still prevents mirror storms.
      this.#start({ explicit: true })
    } else if (expansion !== undefined) {
      this.#runner = undefined
      this.#expanding = true
      this.#complete = false
      this.#current = expansion
        .then(bases => {
          this.#emit(bases)
          this.#complete = true
        })
        .then(
          () => undefined,
          (error: unknown) => {
            this.#expansionFailure = error
            return error
          },
        )
        .finally(() => { this.#expanding = false })
    } else {
      this.#runner = undefined
      this.#expanding = false
      this.#complete = true
      this.#current = Promise.resolve(undefined)
    }
  }

  get bases(): readonly string[] { return this.#bases }
  get expanding(): boolean { return this.#expanding }
  get complete(): boolean { return this.#complete }
  get expansionFailure(): unknown { return this.#expansionFailure }
  get settled(): Promise<void> { return this.#current.then(() => undefined) }

  async all(options: RelayExpansionOptions = {}): Promise<readonly string[]> {
    const failure = await this.#start(options)
    if (this.#bases.length === 0) throw failure ?? new Error('No known relay endpoints')
    return this.#bases
  }

  observe(listener: (bases: readonly string[]) => void): () => void {
    this.#listeners.add(listener)
    listener(this.#bases)
    return () => { this.#listeners.delete(listener) }
  }

  readonly #emit = (additional: readonly string[]): void => {
    if (!additional.some(base => !this.#bases.includes(base))) return
    this.#bases = receiverRelayBases([...this.#bases, ...additional])
    for (const listener of this.#listeners) listener(this.#bases)
  }

  #start(options: RelayExpansionOptions): Promise<unknown> {
    const runner = this.#runner
    if (this.#complete || runner === undefined || this.#expanding) return this.#current
    this.#expanding = true
    let pending: Promise<RelayExpansionStatus | void>
    try {
      pending = runner(this.#emit, options)
    } catch (error) {
      pending = Promise.reject(error)
    }
    const run = pending.then(
      status => {
        this.#expansionFailure = undefined
        if (status !== 'incomplete') this.#complete = true
        return undefined
      },
      (error: unknown) => {
        this.#expansionFailure = error
        return error
      },
    ).finally(() => {
      this.#expanding = false
    })
    this.#current = run
    return run
  }
}

export function relayCandidates(source: RelayCandidateSource): RelayCandidates {
  return source instanceof RelayCandidates ? source : new RelayCandidates(source)
}
