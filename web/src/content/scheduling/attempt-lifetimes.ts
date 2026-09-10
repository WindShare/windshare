import { encodeBase64Url } from '../../crypto/bytes'

interface LeaseAttempts {
  active: number
  readonly idle: Promise<void>
  readonly resolve: () => void
}

/** A winning block and the end of its canceled network work are different facts. */
export class ContentAttemptLifetimes {
  readonly #leases = new Map<string, LeaseAttempts>()

  begin(leaseId: Uint8Array): () => void {
    const key = encodeBase64Url(leaseId)
    let state = this.#leases.get(key)
    if (state === undefined) {
      let resolve!: () => void
      const idle = new Promise<void>((done) => { resolve = done })
      state = { active: 0, idle, resolve }
      this.#leases.set(key, state)
    }
    state.active += 1
    const attempts = state
    return () => {
      attempts.active -= 1
      if (attempts.active !== 0) return
      this.#leases.delete(key)
      attempts.resolve()
    }
  }

  async waitForLeaseIdle(leaseId: Uint8Array): Promise<void> {
    const key = encodeBase64Url(leaseId)
    while (true) {
      const state = this.#leases.get(key)
      if (state === undefined) return
      await state.idle
    }
  }
}
