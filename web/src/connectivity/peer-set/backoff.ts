import type { PeerFailureDecision } from '../peer-failure'
import { calculatePeerRetryDelay, type PeerRecoveryPolicy } from './contract'

type RetryDecision = Extract<PeerFailureDecision, { readonly type: 'retry-attempt' }>

export interface PeerRetryDelay {
  readonly localDelayMilliseconds: number
  readonly authenticatedRetryAfterMilliseconds: number
  readonly effectiveDelayMilliseconds: number
}

/** Authenticated admission limits survive cancellation and replacement of a recovery wave. */
export class PeerRetryBackoff {
  readonly #policy: PeerRecoveryPolicy
  readonly #random: () => number
  #authenticatedNotBefore = 0

  constructor(policy: PeerRecoveryPolicy, random: () => number) {
    this.#policy = policy
    this.#random = random
  }

  retain(decision: RetryDecision, now: number): void {
    if (decision.reason === 'admission-limited') {
      this.#authenticatedNotBefore = Math.max(
        this.#authenticatedNotBefore,
        now + decision.authenticatedRetryAfterMilliseconds,
      )
    }
  }

  retainedDelay(now: number): number {
    return Math.max(0, this.#authenticatedNotBefore - now)
  }

  schedule(now: number, retryOrdinal: number, delayedOpportunityMilliseconds: number): PeerRetryDelay {
    const localDelayMilliseconds = calculatePeerRetryDelay(this.#policy, retryOrdinal, this.#random())
    const authenticatedRetryAfterMilliseconds = this.retainedDelay(now)
    return Object.freeze({
      localDelayMilliseconds,
      authenticatedRetryAfterMilliseconds,
      effectiveDelayMilliseconds: Math.max(
        localDelayMilliseconds, authenticatedRetryAfterMilliseconds, delayedOpportunityMilliseconds,
      ),
    })
  }
}
