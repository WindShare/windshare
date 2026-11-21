import {
  relayCandidates, type RelayCandidateSource, type RelayCandidates, type RelayExpansionOptions,
} from './candidates'
import { firstUsableRelay, RelayEndpointFailure } from './race'
import { systemReconnectClock, type ReconnectClock } from '../recovery-clock'
import {
  isShareRecoveryFailure, isTerminalRecoveryFailure, latestRecoveryRetry, recoveryRetry,
  AllRelaysRestrictedError, RelayCooldownError, type RecoveryRetry,
} from '../recovery-failure'

type EndpointRejection =
  | { readonly kind: 'terminal'; readonly failure: RelayEndpointFailure }
  | { readonly kind: 'cooldown'; readonly failure: RelayEndpointFailure; readonly retry: RecoveryRetry }

/** One share owns relay eligibility from the first dial through later generations. */
export class RelayRecovery {
  readonly candidates: RelayCandidates
  readonly #clock: Pick<ReconnectClock, 'now'>
  readonly #rejections = new Map<string, EndpointRejection>()

  constructor(source: RelayCandidateSource, clock: Pick<ReconnectClock, 'now'> = systemReconnectClock) {
    this.candidates = relayCandidates(source)
    this.#clock = clock
  }

  async race<T>(
    signal: AbortSignal,
    connect: (base: string, signal: AbortSignal) => Promise<T>,
    close: (value: T) => Promise<void>,
    expansion?: RelayExpansionOptions,
  ): Promise<T> {
    try {
      return await firstUsableRelay(this.candidates, signal,
        (base, candidateSignal) => this.attempt(base, candidateSignal, () => connect(base, candidateSignal)),
        close, {
          fatal: isShareRecoveryFailure,
          eligible: base => this.#blockingRejection(base, this.#clock.now()) === undefined,
          ...(expansion === undefined ? {} : { expansion }),
        })
    } catch (failure) {
      signal.throwIfAborted()
      throw this.#selectionFailure(failure)
    }
  }

  async attempt<T>(base: string, signal: AbortSignal, connect: () => Promise<T>): Promise<T> {
    signal.throwIfAborted()
    const rejection = this.#blockingRejection(base, this.#clock.now())
    if (rejection?.kind === 'terminal') throw rejection.failure.cause
    if (rejection?.kind === 'cooldown') throw new RelayCooldownError(rejection.retry, rejection.failure.cause)
    try {
      return await connect()
    } catch (failure) {
      if (!signal.aborted) this.recordFailure(base, failure)
      throw failure
    }
  }

  recordFailure(base: string, cause: unknown): void {
    const previous = this.#rejections.get(base)
    if (previous?.kind === 'terminal' || isShareRecoveryFailure(cause)) return
    if (isTerminalRecoveryFailure(cause)) {
      this.#rejections.set(base, { kind: 'terminal', failure: new RelayEndpointFailure(base, cause) })
      return
    }
    const retry = recoveryRetry(cause, this.#clock.now())
    if (retry !== undefined && latestRecoveryRetry(previous?.retry, retry) === retry) {
      this.#rejections.set(base, { kind: 'cooldown', failure: new RelayEndpointFailure(base, cause), retry })
    }
  }

  nextRetry(): RecoveryRetry | undefined {
    // Discovery can still supply a route that has never been attempted.
    if (this.candidates.expanding) return undefined
    const now = this.#clock.now()
    let earliest: RecoveryRetry | undefined
    for (const base of this.candidates.bases) {
      const rejection = this.#blockingRejection(base, now)
      if (rejection === undefined) return undefined
      if (rejection.kind === 'cooldown' && (earliest === undefined || rejection.retry.retryAt < earliest.retryAt)) {
        earliest = rejection.retry
      }
    }
    return earliest
  }

  #selectionFailure(failure: unknown): unknown {
    if (isShareRecoveryFailure(failure)) return failure
    const rejections = this.candidates.bases.map(base => this.#rejections.get(base))
    if (rejections.length > 0 && rejections.every(rejection => rejection?.kind === 'terminal')) {
      if (rejections.length === 1) return rejections[0]!.failure
      return new AggregateError(rejections.map(rejection => rejection.failure),
        'All relay endpoints rejected this share', { cause: failure })
    }
    const cooldowns = rejections.filter(rejection => rejection?.kind === 'cooldown')
    const allRestricted = cooldowns.length > 0 && cooldowns.length === rejections.length &&
      cooldowns.every(({ retry }) => retry.reason === 'authorization')
    // Pending discovery and expired cooldowns can still yield other routes.
    const retry = allRestricted ? this.nextRetry() : undefined
    if (retry !== undefined) return new AllRelaysRestrictedError(retry.retryAt, failure)
    if (cooldowns.length > 0 && isTerminalRecoveryFailure(failure)) {
      return new AggregateError([failure, ...cooldowns.map(rejection => rejection.failure)],
        'Some relay endpoints are cooling down', { cause: failure })
    }
    return failure
  }

  #blockingRejection(base: string, now: number): EndpointRejection | undefined {
    const rejection = this.#rejections.get(base)
    return rejection?.kind === 'terminal' || (rejection?.kind === 'cooldown' && rejection.retry.retryAt > now)
      ? rejection : undefined
  }
}
