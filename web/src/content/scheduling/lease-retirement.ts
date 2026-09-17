import { V2SessionRuntimeError } from '../../session/v2-runtime-types'
import { delayWithAbort, operationDeadlineSignal } from './deadlines'

export const LEASE_RETIREMENT_WAIT_MILLISECONDS = 30_000
// Cleanup must leave operation capacity for reads even while a relay is stalled.
export const MAXIMUM_ACTIVE_LEASE_RETIREMENTS = 16
export const MAXIMUM_QUEUED_LEASE_RETIREMENTS = 256
const RETRY_INITIAL_MILLISECONDS = 250
const RETRY_MAXIMUM_MILLISECONDS = 2_000

export type LeaseRetirementObservation = Readonly<{
  leaseId: string
  attempt: number
}> & (
  | Readonly<{ transition: 'waiting_for_reads' | 'deferred_for_reads' | 'released' }>
  | Readonly<{ transition: 'retrying'; failure: unknown }>
  | Readonly<{
      transition: 'abandoned'
      reason: 'service_closed' | 'deadline' | 'remote_failure' | 'barrier_failure' | 'capacity'
      failure: unknown
    }>
)

export interface LeaseRetirementOwner {
  readonly signal: AbortSignal
  readonly observe?: (observation: LeaseRetirementObservation) => void
}

interface PendingLeaseRetirement {
  readonly leaseId: string
  readonly release: (signal: AbortSignal) => Promise<void>
}

/** The session owns remote reclamation after a consumer releases its local lease. */
export class LeaseRetirementQueue {
  readonly #owner: LeaseRetirementOwner
  readonly #pending: PendingLeaseRetirement[] = []
  readonly #active = new Set<Promise<void>>()

  constructor(owner: LeaseRetirementOwner) {
    this.#owner = owner
    owner.signal.addEventListener('abort', () => {
      for (const item of this.#pending.splice(0)) this.#abandon(item.leaseId, 'service_closed')
    }, { once: true })
  }

  retire(leaseId: string, release: (signal: AbortSignal) => Promise<void>): void {
    if (this.#owner.signal.aborted) {
      this.#abandon(leaseId, 'service_closed')
      return
    }
    if (this.#pending.length >= MAXIMUM_QUEUED_LEASE_RETIREMENTS) {
      // Renewal has stopped before handoff. Sender TTL still reclaims a lease
      // when cleanup capacity is exhausted; file settlement must remain local.
      this.#abandon(leaseId, 'capacity')
      return
    }
    this.#pending.push({ leaseId, release })
    this.#pump()
  }

  #pump(): void {
    while (!this.#owner.signal.aborted && this.#active.size < MAXIMUM_ACTIVE_LEASE_RETIREMENTS) {
      const item = this.#pending.shift()
      if (item === undefined) return
      // Reserve the slot before injected I/O or observers can reenter admission.
      const task = Promise.resolve().then(() => retireRemoteLease(item.leaseId, this.#owner, item.release)).finally(() => {
        this.#active.delete(task)
        this.#pump()
      })
      this.#active.add(task)
    }
  }

  #abandon(leaseId: string, reason: 'service_closed' | 'capacity'): void {
    observeLeaseRetirement(this.#owner, {
      leaseId, attempt: 0, transition: 'abandoned', reason,
      failure: reason === 'service_closed'
        ? this.#owner.signal.reason
        : new Error('Remote lease retirement queue is full'),
    })
  }
}

/** Remote resource reclamation cannot invalidate authenticated, committed output. */
export async function retireRemoteLease(
  leaseId: string,
  owner: LeaseRetirementOwner,
  release: (signal: AbortSignal) => Promise<void>,
): Promise<void> {
  const deadline = operationDeadlineSignal(owner.signal, LEASE_RETIREMENT_WAIT_MILLISECONDS,
    new DOMException('Revision lease retirement timed out', 'TimeoutError'))
  let attempt = 0
  let delay = RETRY_INITIAL_MILLISECONDS
  try {
    while (true) {
      deadline.signal.throwIfAborted()
      attempt += 1
      try {
        await release(deadline.signal)
        observeLeaseRetirement(owner, { leaseId, attempt, transition: 'released' })
        return
      } catch (failure) {
        if (deadline.signal.aborted || !(failure instanceof V2SessionRuntimeError) || failure.scope !== 'lane') {
          throw failure
        }
        // RELEASE is idempotent within its ProtocolSession, even if the sender
        // reclaimed the lease before its reply was lost. OPEN has no such guarantee.
        observeLeaseRetirement(owner, { leaseId, attempt, transition: 'retrying', failure })
        await delayWithAbort(delay, deadline.signal)
        delay = Math.min(delay * 2, RETRY_MAXIMUM_MILLISECONDS)
      }
    }
  } catch (failure) {
    // Renewal has already stopped. Unconfirmed reclamation is bounded by the
    // sender's lease TTL or session teardown, independently of file settlement.
    observeLeaseRetirement(owner, { leaseId, attempt, transition: 'abandoned',
      reason: retirementFailureReason(owner.signal, deadline.signal),
      failure })
  } finally {
    deadline.close()
  }
}

function retirementFailureReason(lifetime: AbortSignal, deadline: AbortSignal): 'service_closed' | 'deadline' | 'remote_failure' {
  if (lifetime.aborted) return 'service_closed'
  return deadline.aborted ? 'deadline' : 'remote_failure'
}

export function observeLeaseRetirement(owner: LeaseRetirementOwner, observation: LeaseRetirementObservation): void {
  try { owner.observe?.(observation) } catch { /* Diagnostics cannot change lease ownership. */ }
}
