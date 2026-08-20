import {
  ArtifactPlanningContractError,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  sameArtifactChoiceSemantics,
  sameMaterializationRouteIdentity,
  type ArtifactChoiceReconcileOutcome,
  type ArtifactOffers,
  type ArtifactResolutionObservation,
  type MaterializationRouteIdentity,
  type OfferedArtifactChoice,
} from '../../output/planning'
import type { ProjectionEpoch, SelectionProjectionState } from '../../transfer/projection'
import { V2ActivationStateContractError } from './activation-model'
import type { V2ActiveProjection } from './projection-observation'

export interface V2AuthorityProjectionPublication {
  readonly observationRevision: number
  readonly state: SelectionProjectionState
  readonly offers: ArtifactOffers
}

export interface AuthorityPlanningRequest {
  readonly revision: number
  readonly active: V2ActiveProjection
  readonly state: SelectionProjectionState
}

export interface AuthorityPlanningSubject {
  readonly key: object
  readonly choice: OfferedArtifactChoice['choice']
  readonly installedRoute: MaterializationRouteIdentity
  readonly selectionDigest: string
}

export type ArtifactOfferPlanner = typeof offerArtifacts
export type ArtifactChoiceReconciler = typeof reconcileArtifactChoice

export interface AuthorityPlanningPipelineOptions {
  readonly currentProjection: () => V2ActiveProjection | undefined
  readonly offersApplied: (request: AuthorityPlanningRequest, offers: ArtifactOffers) => void
  readonly planningFailed: (error: unknown) => void
  readonly reconciliationApplied: (
    subject: AuthorityPlanningSubject,
    request: AuthorityPlanningRequest,
    outcome: AuthorityPlanningResult,
  ) => void
  readonly reconciliationFailed: (subject: AuthorityPlanningSubject, error: unknown) => void
  readonly reconciliationEvidenceAdvanced: (subject: AuthorityPlanningSubject) => void
  readonly planner?: ArtifactOfferPlanner
  readonly reconciler?: ArtifactChoiceReconciler
}

export type AuthorityPlanningResult =
  | Exclude<ArtifactChoiceReconcileOutcome, { kind: 'resolved' | 'invalidated' }>
  | Readonly<{
      kind: 'invalidated'
      reason: Extract<ArtifactChoiceReconcileOutcome, { kind: 'invalidated' }>['reason'] |
        'installed-route-changed'
    }>
  | Readonly<{
      kind: 'resolved'
      action: Extract<ArtifactChoiceReconcileOutcome, { kind: 'resolved' }>['action']
    }>

interface ReconciliationLedgerEntry {
  readonly request: AuthorityPlanningRequest
  outcome?: ArtifactChoiceReconcileOutcome
}

interface ReconciliationEpochLedger {
  readonly entries: ReconciliationLedgerEntry[]
  validatedRevision: number
  previousObservation: ArtifactResolutionObservation | null
}

/**
 * Orders observational evidence before the coordinator may act on it. Keeping
 * this ledger outside activation state prevents completion order from becoming
 * authority while leaving every semantic decision with the coordinator.
 */
export class AuthorityPlanningPipeline {
  readonly #options: AuthorityPlanningPipelineOptions
  readonly #planner: ArtifactOfferPlanner
  readonly #reconciler: ArtifactChoiceReconciler
  readonly #ledgers = new WeakMap<object, Map<ProjectionEpoch, ReconciliationEpochLedger>>()
  #revision = 0
  #latestAppliedRevision = 0
  #latestOffers: Readonly<{
    request: AuthorityPlanningRequest
    offers: ArtifactOffers
  }> | undefined

  constructor(options: AuthorityPlanningPipelineOptions) {
    this.#options = options
    this.#planner = options.planner ?? offerArtifacts
    this.#reconciler = options.reconciler ?? reconcileArtifactChoice
  }

  get currentRevision(): number {
    return this.#revision
  }

  get latestOffers(): Readonly<{
    request: AuthorityPlanningRequest
    offers: ArtifactOffers
  }> | undefined {
    return this.#latestOffers
  }

  observe(
    active: V2ActiveProjection,
    subject?: AuthorityPlanningSubject,
  ): AuthorityPlanningRequest {
    const request: AuthorityPlanningRequest = Object.freeze({
      revision: ++this.#revision,
      active,
      state: active.state,
    })
    this.#planner(request.state.projection, request.state.discovery, active.environment).then(
      offers => this.#applyOffers(request, offers),
      error => this.#planningFailed(request, error),
    ).catch(() => undefined)
    if (subject !== undefined) this.reconcile(request, subject)
    return request
  }

  reconcile(request: AuthorityPlanningRequest, subject: AuthorityPlanningSubject): void {
    const ledger = this.#ledger(subject.key, request.active.epoch)
    const entry: ReconciliationLedgerEntry = { request }
    ledger.entries.push(entry)
    const previousObservation = ledger.previousObservation
    this.#reconciler({
      choice: subject.choice,
      preferredRoute: subject.installedRoute,
      expectedSelectionDigest: subject.selectionDigest,
      projection: request.state.projection,
      discovery: request.state.discovery,
      environment: request.active.environment,
      previousObservation,
    }).then(
      outcome => this.#completeReconciliation(subject, ledger, entry, outcome),
      error => this.#reconciliationFailed(subject, ledger, entry, error),
    ).catch(() => undefined)
  }

  beginReplacement(advanceRevision = true): void {
    if (advanceRevision) ++this.#revision
    this.#latestOffers = undefined
  }

  invalidated(): void {
    ++this.#revision
    this.#latestOffers = undefined
  }

  isCurrent(request: AuthorityPlanningRequest): boolean {
    const current = this.#options.currentProjection()
    return request.revision === this.#revision && current === request.active &&
      request.active.state === request.state && !request.active.controller.signal.aborted
  }

  readyForCommit(subject: AuthorityPlanningSubject, epoch: ProjectionEpoch, revision: number): boolean {
    const ledger = this.#ledgers.get(subject.key)?.get(epoch)
    return revision === this.#revision && this.#latestAppliedRevision === this.#revision &&
      ledger !== undefined && ledger.validatedRevision >= revision
  }

  #applyOffers(request: AuthorityPlanningRequest, offers: ArtifactOffers): void {
    if (!this.isCurrent(request)) return
    if (offers.projectionEpoch !== request.state.projection.epoch ||
        offers.selectionDigest !== request.state.projection.selectionDigest) {
      this.#planningFailed(
        request,
        new V2ActivationStateContractError('artifact offers escaped their planning observation'),
      )
      return
    }
    this.#latestAppliedRevision = request.revision
    this.#latestOffers = Object.freeze({ request, offers })
    this.#options.offersApplied(request, offers)
  }

  #completeReconciliation(
    subject: AuthorityPlanningSubject,
    ledger: ReconciliationEpochLedger,
    entry: ReconciliationLedgerEntry,
    outcome: ArtifactChoiceReconcileOutcome,
  ): void {
    entry.outcome = outcome
    try {
      this.#validateLedger(ledger)
    } catch (error) {
      this.#options.reconciliationFailed(subject, error)
      return
    }
    if (this.isCurrent(entry.request)) {
      this.#options.reconciliationApplied(
        subject,
        entry.request,
        classifyReconciliation(subject, outcome),
      )
    }
    this.#options.reconciliationEvidenceAdvanced(subject)
  }

  #reconciliationFailed(
    subject: AuthorityPlanningSubject,
    ledger: ReconciliationEpochLedger,
    entry: ReconciliationLedgerEntry,
    error: unknown,
  ): void {
    if (error instanceof ArtifactPlanningContractError || this.isCurrent(entry.request)) {
      this.#options.reconciliationFailed(subject, error)
      return
    }
    const index = ledger.entries.indexOf(entry)
    if (index >= 0) ledger.entries.splice(index, 1)
    try {
      this.#validateLedger(ledger)
    } catch (validationError) {
      this.#options.reconciliationFailed(subject, validationError)
      return
    }
    this.#options.reconciliationEvidenceAdvanced(subject)
  }

  #planningFailed(request: AuthorityPlanningRequest, error: unknown): void {
    if (this.isCurrent(request)) this.#options.planningFailed(error)
  }

  #ledger(key: object, epoch: ProjectionEpoch): ReconciliationEpochLedger {
    let byEpoch = this.#ledgers.get(key)
    if (byEpoch === undefined) {
      byEpoch = new Map()
      this.#ledgers.set(key, byEpoch)
    }
    let ledger = byEpoch.get(epoch)
    if (ledger === undefined) {
      ledger = { entries: [], validatedRevision: 0, previousObservation: null }
      byEpoch.set(epoch, ledger)
    }
    return ledger
  }

  #validateLedger(ledger: ReconciliationEpochLedger): void {
    let previous: ArtifactResolutionObservation | null = null
    let validatedRevision = 0
    for (const entry of ledger.entries) {
      if (entry.outcome === undefined) break
      const observation = entry.outcome.observation
      if (previous !== null) assertOrderedResolutionEvidence(previous, observation)
      previous = observation
      validatedRevision = entry.request.revision
    }
    ledger.previousObservation = previous
    ledger.validatedRevision = validatedRevision
  }
}

export function planningSubject(input: Readonly<{
  key?: object
  choice: OfferedArtifactChoice['choice']
  installedRoute: MaterializationRouteIdentity
  selectionDigest: string
}>): AuthorityPlanningSubject {
  return Object.freeze({
    key: input.key ?? input,
    choice: input.choice,
    installedRoute: input.installedRoute,
    selectionDigest: input.selectionDigest,
  })
}

function classifyReconciliation(
  subject: AuthorityPlanningSubject,
  outcome: ArtifactChoiceReconcileOutcome,
): AuthorityPlanningResult {
  if (outcome.kind !== 'resolved') return outcome
  if (!sameArtifactChoiceSemantics(subject.choice, outcome.action.choice)) {
    return Object.freeze({ kind: 'invalidated', reason: 'semantic-route-unavailable' })
  }
  if (!sameMaterializationRouteIdentity(
    subject.installedRoute,
    materializationRouteIdentity(outcome.action.route),
  )) {
    return Object.freeze({ kind: 'invalidated', reason: 'installed-route-changed' })
  }
  return Object.freeze({ kind: 'resolved', action: outcome.action })
}

function assertOrderedResolutionEvidence(
  previous: ArtifactResolutionObservation,
  current: ArtifactResolutionObservation,
): void {
  if (previous.projectionEpoch !== current.projectionEpoch) return
  if (previous.selectionDigest !== current.selectionDigest) {
    throw new ArtifactPlanningContractError('same-epoch-selection-digest-changed')
  }
  if (previous.resolvedArtifactDigest !== null &&
      previous.resolvedArtifactDigest !== current.resolvedArtifactDigest) {
    throw new ArtifactPlanningContractError('same-epoch-resolved-artifact-digest-changed')
  }
}
