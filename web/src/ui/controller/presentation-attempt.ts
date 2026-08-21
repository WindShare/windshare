import {
  unclassifiedFailureFact,
  type FailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureStage,
  type IncidentScopeHandle,
  type IncidentScopeKind,
  type IncidentScopeOwner,
  type PresentationBoundary,
  type PresentationExclusionReason,
  type PresentationOutcome,
  type RecoveryDisposition,
} from '../../diagnostics/incident'
import {
  excludedPresentationDecision,
  incidentPresentationDecision,
} from '../../diagnostics/incident/presentation'
import {
  createAttemptOutputFailureCapability,
  createLateOutputCleanupCapability,
  type AttemptOutputFailureCapability,
  type LateOutputFailureConsequenceCapability,
  type OutputFailureSinks,
} from '../../output/diagnostics'
import type { V2ReceiverIncidentPort } from './contracts'

/**
 * Presentation owners retain this capability rather than a reporter reference in
 * asynchronous callbacks. The attempt keeps every diagnostic exception outside
 * product authority while preserving owner-only close and explicit causality.
 */
export class V2PresentationAttempt {
  readonly #incidents: V2ReceiverIncidentPort | undefined
  readonly #scope: IncidentScopeOwner | undefined
  readonly #outputFailures: AttemptOutputFailureCapability
  #decisionSettled = false
  #incidentSettled = false
  #lateCleanupCapabilityIssued = false
  #closed = false

  constructor(
    incidents: V2ReceiverIncidentPort | undefined,
    kind: IncidentScopeKind,
  ) {
    this.#incidents = incidents
    try {
      this.#scope = incidents?.openScope(kind)
    } catch {
      // Diagnostics cannot prevent an accepted product attempt from starting.
    }
    this.#outputFailures = createAttemptOutputFailureCapability(this.#scope?.handle)
  }

  get decisionSettled(): boolean {
    return this.#decisionSettled
  }

  get incidentSettled(): boolean {
    return this.#incidentSettled
  }

  get handle(): IncidentScopeHandle | undefined {
    return this.#scope?.handle
  }

  get outputFailures(): OutputFailureSinks {
    return this.#outputFailures.sinks
  }

  get outputFailureTrigger(): FailureFactRef | undefined {
    return this.#outputFailures.firstContributor()
  }

  record(
    fact: FailureFact,
    relation: FailureFactRelation,
  ): FailureFactRef | undefined {
    try {
      return this.#scope?.facts.record(fact, relation)
    } catch {
      return undefined
    }
  }

  recordUnclassified(
    stage: FailureStage,
    relation: FailureFactRelation,
    recoveryDisposition: RecoveryDisposition =
      relation === 'contributor' ? 'terminal' : 'none',
  ): FailureFactRef | undefined {
    return this.record(
      unclassifiedFailureFact({ stage, recoveryDisposition }),
      relation,
    )
  }

  incident(
    boundary: PresentationBoundary,
    outcome: PresentationOutcome,
    trigger: FailureFactRef,
  ): void {
    if (this.#decisionSettled) return
    this.#decisionSettled = true
    this.#incidentSettled = true
    if (this.#scope === undefined) return
    try {
      this.#incidents?.submitDecision(
        this.#scope.handle,
        incidentPresentationDecision(boundary, outcome, trigger),
      )
    } catch {
      // Reporter failure cannot change the already-authorized presentation.
    }
  }

  excluded(
    boundary: PresentationBoundary,
    reason: PresentationExclusionReason,
  ): void {
    if (this.#decisionSettled) return
    this.#decisionSettled = true
    if (this.#scope === undefined) return
    try {
      this.#incidents?.submitDecision(
        this.#scope.handle,
        excludedPresentationDecision(boundary, reason),
      )
    } catch {
      // Reporter failure cannot change the already-authorized presentation.
    }
  }

  createLateCleanupCapability(): LateOutputFailureConsequenceCapability | undefined {
    if (
      !this.#incidentSettled ||
      this.#scope === undefined ||
      this.#lateCleanupCapabilityIssued
    ) {
      return undefined
    }
    this.#lateCleanupCapabilityIssued = true
    return createLateOutputCleanupCapability(this.#scope.handle)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#outputFailures.revoke()
    try {
      this.#scope?.close()
    } catch {
      // Diagnostic close cannot delay product cleanup or publication.
    }
  }
}
