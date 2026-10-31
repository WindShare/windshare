import {
  createFailureFactAccumulator,
  type FailureFactAccumulator,
  type SealedFailureFacts,
} from './aggregate'
import type { IncidentPolicy } from './policy'
import {
  isPresentationDecision,
  presentationBoundaryForScope,
  type IncidentPresentationDecision,
  type PresentationDecision,
} from './presentation'
import {
  failureFactForRef,
  failureFactRelationForRef,
  type FailureFactRef,
  type IncidentScopeHandle,
  type IncidentScopeIdentity,
} from './scope'

export const MAX_PENDING_INCIDENT_CONTEXTS = 16

type PendingFactContext =
  | Readonly<{ kind: 'empty' }>
  | Readonly<{ kind: 'retained'; accumulator: FailureFactAccumulator }>
  | Readonly<{ kind: 'omitted' }>

interface PendingScope {
  readonly handle: IncidentScopeHandle
  context: PendingFactContext
  contributorCount: bigint
  consequenceCount: bigint
  decision?: IncidentPresentationDecision
  sealRequested: boolean
}

export class PendingIncidentScopes {
  // Live operation capabilities own decision eligibility. The bounded context
  // cache may forget supporting facts without revoking those capabilities.
  readonly #scopes = new WeakMap<IncidentScopeIdentity, PendingScope>()
  readonly #contexts = new Map<IncidentScopeIdentity, PendingScope>()
  readonly #policy: IncidentPolicy
  readonly #onSeal: (decision: IncidentPresentationDecision, facts: SealedFailureFacts) => void

  constructor(
    policy: IncidentPolicy,
    onSeal: (decision: IncidentPresentationDecision, facts: SealedFailureFacts) => void,
  ) {
    this.#policy = policy
    this.#onSeal = onSeal
  }

  register(handle: IncidentScopeHandle): void {
    this.#scopes.set(handle.identity, {
      handle,
      context: { kind: 'empty' },
      contributorCount: 0n,
      consequenceCount: 0n,
      sealRequested: false,
    })
  }

  record(ref: FailureFactRef): boolean {
    const state = this.#scopes.get(ref.scope)
    if (state === undefined) return false
    if (state.context.kind === 'empty') {
      while (this.#contexts.size >= MAX_PENDING_INCIDENT_CONTEXTS) this.#evictContext()
      state.context = {
        kind: 'retained',
        accumulator: createFailureFactAccumulator(ref.scope, this.#policy),
      }
      this.#contexts.set(ref.scope, state)
    }
    if (state.context.kind === 'retained') state.context.accumulator.record(ref)
    if (failureFactRelationForRef(ref) === 'contributor') state.contributorCount++
    else state.consequenceCount++
    return true
  }

  decide(handle: IncidentScopeHandle, decision: PresentationDecision): void {
    const state = this.#scopes.get(handle.identity)
    if (state?.handle !== handle || state.decision !== undefined) return
    if (!isPresentationDecision(decision) ||
        decision.boundary !== presentationBoundaryForScope(handle.identity.scopeKind)) return
    if (decision.kind === 'excluded') {
      this.#remove(state)
      return
    }
    if (decision.trigger.scope !== handle.identity) return
    state.decision = decision
    this.#trySeal(state)
  }

  requestSeal(identity: IncidentScopeIdentity): void {
    const state = this.#scopes.get(identity)
    if (state === undefined) return
    state.sealRequested = true
    this.#trySeal(state)
  }

  #evictContext(): void {
    const oldest = this.#contexts.values().next().value
    if (oldest === undefined) return
    if (oldest.decision !== undefined) {
      oldest.sealRequested = true
      this.#trySeal(oldest)
      return
    }
    this.#contexts.delete(oldest.handle.identity)
    oldest.context = { kind: 'omitted' }
  }

  #trySeal(state: PendingScope): void {
    if (!state.sealRequested || state.decision === undefined) return
    // Retire before invoking sinks: reentrant decisions and later deadlines
    // cannot publish a second root for the same operation.
    this.#remove(state)
    try {
      const facts = state.context.kind === 'retained'
        ? state.context.accumulator.seal(state.decision.trigger)
        : sealOmittedContext(state, state.decision.trigger)
      this.#onSeal(state.decision, facts)
    } catch {
      // Diagnostic projection and delivery cannot change the owned outcome.
    }
  }

  #remove(state: PendingScope): void {
    this.#scopes.delete(state.handle.identity)
    this.#contexts.delete(state.handle.identity)
  }
}

function sealOmittedContext(state: PendingScope, trigger: FailureFactRef): SealedFailureFacts {
  if (state.contributorCount === 0n) throw new TypeError('Incident trigger was not observed')
  // The owner still holds its nominated trigger. Only supporting observations
  // are omitted; the reporter accounts them once, when this incident is sealed.
  return Object.freeze({
    scope: state.handle.identity,
    trigger: Object.freeze({ ref: trigger, fact: failureFactForRef(trigger) }),
    factCount: state.contributorCount + state.consequenceCount,
    contributorBuckets: Object.freeze([]),
    contributorOverflowCount: state.contributorCount - 1n,
    consequenceBuckets: Object.freeze([]),
    consequenceOverflowCount: state.consequenceCount,
  })
}
