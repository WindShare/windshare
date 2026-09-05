import { abortOwner, authoritativeAfterCancellation, localContractResult, normalizeAttemptResult, waitForAbort } from './attempt-result'
import type { V2PeerAttemptHandle, V2PeerRecoveryClock, V2PeerRecoveryAttemptRace as AttemptRace } from './contract'
import type { V2PeerAttemptCancellationOwner } from '../v2-peer-failure'
const NEVER: Promise<never> = new Promise(() => undefined)
export async function racePeerAttempt(
  attempt: V2PeerAttemptHandle, clock: V2PeerRecoveryClock,
  remaining: { readonly wave: number; readonly session: number }, signal: AbortSignal,
  cancel: (attempt: V2PeerAttemptHandle, owner: V2PeerAttemptCancellationOwner) => void,
): Promise<AttemptRace> {
    const exhaustion = remaining.session <= remaining.wave
      ? 'session-elapsed-budget'
      : 'wave-elapsed-budget'
    const timer = new AbortController()
    const budget = clock.sleep(Math.min(remaining.wave, remaining.session), timer.signal)
      .then((): AttemptRace => ({ type: 'budget-expired', exhaustion }))
      .catch(() => timer.signal.aborted ? NEVER : Promise.resolve({
        type: 'result',
        result: localContractResult(),
      } as AttemptRace))
    const ownerCancellation = waitForAbort(signal)
      .then((): AttemptRace => ({ type: 'owner-cancelled' }))
    const result = Promise.resolve(attempt.result)
      .then((value): AttemptRace => ({ type: 'result', result: normalizeAttemptResult(value) }))
      .catch((): AttemptRace => ({ type: 'result', result: localContractResult() }))

    const winner = await Promise.race([result, budget, ownerCancellation])
    timer.abort()
    if (winner.type === 'result') return winner

    cancel(attempt, winner.type === 'budget-expired' ? 'recovery-budget' : abortOwner(signal))
    const joined = await result
    if (joined.type === 'result' && authoritativeAfterCancellation(joined.result)) return joined
    return winner
}

export function cancelPeerAttempt(attempt: V2PeerAttemptHandle | undefined, owner: V2PeerAttemptCancellationOwner): void {
    try {
      attempt?.cancel(owner)
    } catch {
      // Attempt cleanup is joined below; a throwing adapter cannot change policy authority.
    }
  }
