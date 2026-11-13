export const MAXIMUM_RECEIVER_RELAYS = 8
export type RelayCandidateSource = readonly string[] | RelayCandidates

export function receiverRelayBases(values: readonly string[]): readonly string[] {
  const unique = [...new Set(values.map(value => value.trim()))]
  if (unique.length === 0 || unique.length > MAXIMUM_RECEIVER_RELAYS || unique.includes('')) throw new RangeError(`A receiver needs between 1 and ${MAXIMUM_RECEIVER_RELAYS} relay endpoints`)
  return Object.freeze(unique)
}

/** Owns one bounded expansion, independently of retries and protocol generations. */
export class RelayCandidates {
  #bases: readonly string[]
  readonly #listeners = new Set<(bases: readonly string[]) => void>()
  readonly settled: Promise<void>
  #failure: unknown

  constructor(initial: readonly string[], expansion?: Promise<readonly string[]>) {
    this.#bases = initial.length === 0 && expansion ? [] : receiverRelayBases(initial)
    this.settled = expansion ? expansion.then(additional => {
      this.#bases = receiverRelayBases([...this.#bases, ...additional])
      for (const listener of this.#listeners) listener(this.#bases)
    }).catch((error: unknown) => { this.#failure = error }) : Promise.resolve()
  }

  get bases(): readonly string[] { return this.#bases }

  async all(): Promise<readonly string[]> {
    await this.settled
    if (this.#bases.length === 0) throw this.#failure ?? new Error('No known relay endpoints')
    return this.#bases
  }

  observe(listener: (bases: readonly string[]) => void): () => void {
    this.#listeners.add(listener)
    listener(this.#bases)
    return () => { this.#listeners.delete(listener) }
  }

  filter(accept: (base: string) => boolean): RelayCandidates {
    return new RelayCandidates(this.#bases.filter(accept), this.all().then(bases => bases.filter(accept)))
  }
}

export function relayCandidates(source: RelayCandidateSource): RelayCandidates {
  return source instanceof RelayCandidates ? source : new RelayCandidates(source)
}
