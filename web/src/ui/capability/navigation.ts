export type CapabilityJoinOutcome = 'joined' | 'failed' | 'blocked'
export type CapabilityNavigationDecision = 'opened' | 'reused' | 'superseded' | 'blocked'

interface Navigation {
  readonly fingerprint: Promise<string | undefined>
}

/** Keeps repeated capability input from replacing an in-flight or authenticated join. */
export class CapabilityNavigation {
  #current: Navigation | undefined
  #sequence = 0
  readonly #canReuse: () => boolean

  constructor(canReuse: () => boolean) {
    this.#canReuse = canReuse
  }

  open(
    fingerprint: Promise<string | undefined>,
    join: () => Promise<CapabilityJoinOutcome>,
  ): Promise<CapabilityNavigationDecision> {
    const sequence = ++this.#sequence
    const current = this.#current
    const next = { fingerprint }
    if (current === undefined) return this.#start(next, join)
    return Promise.all([current.fingerprint, fingerprint]).then(async ([previousKey, nextKey]) => {
      if (this.#sequence !== sequence) return 'superseded'
      if (this.#current === current && nextKey !== undefined && previousKey === nextKey && this.#canReuse()) {
        return 'reused'
      }
      return this.#start(next, join)
    })
  }

  clear(): void {
    this.#sequence += 1
    this.#current = undefined
  }

  async #start(next: Navigation, join: () => Promise<CapabilityJoinOutcome>): Promise<CapabilityNavigationDecision> {
    const previous = this.#current
    this.#current = next
    try {
      const outcome = await join()
      if (this.#current === next && outcome !== 'joined') {
        this.#current = outcome === 'blocked' ? previous : undefined
      }
      return outcome === 'blocked' ? 'blocked' : 'opened'
    } catch (error) {
      if (this.#current === next) this.#current = undefined
      throw error
    }
  }
}
