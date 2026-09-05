import type { PeerAttemptBudget } from './budget'
import type { V2PeerRecoveryExhaustion, V2PeerRecoveryPolicy } from './contract'

export const PEER_DELAYED_OPPORTUNITY_MILLISECONDS = 10_000
export type PeerPathNotice = 'mapping-ready' | 'network-changed'

/** A notice changes the next opportunity, never the wave's elapsed or attempt authority. */
export class PeerRecoveryWave {
  attempts = 0
  #notice: PeerPathNotice | undefined
  readonly #reservation: ReturnType<PeerAttemptBudget['reserveElapsed']>

  readonly ordinal: number
  readonly startedAt: number
  readonly policy: V2PeerRecoveryPolicy
  readonly budget: PeerAttemptBudget

  constructor(ordinal: number, startedAt: number,
    policy: V2PeerRecoveryPolicy, budget: PeerAttemptBudget) {
    this.ordinal = ordinal
    this.startedAt = startedAt
    this.policy = policy
    this.budget = budget
    this.#reservation = budget.reserveElapsed(startedAt, policy.waveElapsedBudgetMilliseconds)
  }

  notice(notice: PeerPathNotice): void { this.#notice ??= notice }

  delayedOpportunity(now: number): number {
    if (this.#notice !== undefined || this.attempts !== this.policy.waveMaxAttempts - 1) return 0
    // Keep the last fresh start available for late mapping while preserving a full
    // attempt window. Tiny injected wave budgets have no delay capacity.
    const delay = Math.min(PEER_DELAYED_OPPORTUNITY_MILLISECONDS, Math.max(0,
      this.policy.waveElapsedBudgetMilliseconds - this.opportunityMilliseconds))
    return Math.max(0, this.startedAt + delay - now)
  }

  get opportunityMilliseconds(): number {
    return Math.min(this.policy.waveElapsedBudgetMilliseconds,
      this.policy.negotiationBudgetMilliseconds + this.policy.admissionBudgetMilliseconds)
  }

  remaining(now: number): { readonly wave: number; readonly session: number } {
    return {
      wave: Math.max(0, this.policy.waveElapsedBudgetMilliseconds - (now - this.startedAt)),
      session: this.#reservation.milliseconds === this.policy.waveElapsedBudgetMilliseconds
        ? Number.POSITIVE_INFINITY
        : Math.max(0, this.#reservation.milliseconds - (now - this.startedAt)),
    }
  }

  exhaustion(now: number, reserve = false): V2PeerRecoveryExhaustion | undefined {
    if (reserve) {
      if (this.budget.available(now).attempts === 0) return 'session-attempt-budget'
      if (this.attempts >= this.policy.waveMaxAttempts) return 'wave-attempt-budget'
      if (this.remaining(now).wave < this.opportunityMilliseconds) return 'wave-elapsed-budget'
    }
    const remaining = this.remaining(now)
    if (remaining.session <= 0) return 'session-elapsed-budget'
    return remaining.wave <= 0 ? 'wave-elapsed-budget' : undefined
  }

  release(now: number): void { this.#reservation.release(now - this.startedAt, now) }
}
