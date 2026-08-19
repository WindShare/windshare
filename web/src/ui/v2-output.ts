import type { DomainTraceSource } from '../diagnostics/trace/ports'
import {
  offerArtifacts,
  type ArtifactAction,
  type ArtifactOffers,
  type ArtifactOperation,
  type EnvironmentOffers,
  type OfferComputedDecision,
  type OfferDisabledDecision,
} from '../output/planning'
import {
  lifecycleDeadline,
  type ReceiveLifecycleState,
} from '../output/workspace'
import type {
  ArtifactSpec,
  MaterializationPlan,
  ReceiveIntent,
} from '../transfer/intent'
import type {
  ProjectionEpoch,
  SelectionProjectionState,
} from '../transfer/projection'
import type { TransferJobResult } from '../transfer/job/contract'
import {
  presentArtifactOffers,
  type ArtifactOfferPresentation,
} from './v2-artifact-presentation'
import {
  presentReceiveLifecycle,
  type ReceiveLifecyclePresentation,
  type V2ActiveReceiveControl,
  type WorkspaceUsage,
} from './v2-lifecycle-presentation'
import {
  presentTransferResult,
  type TransferResultPresentation,
} from './v2-transfer-result'

export interface V2OutputPresentationSnapshot {
  readonly projection: SelectionProjectionState | null
  readonly offers: ArtifactOffers | null
  readonly offerPresentation: ArtifactOfferPresentation | null
  readonly chosenAction: ArtifactAction | null
  readonly chosenArtifactKind: ArtifactSpec['kind'] | null
  readonly chosenArtifact: ArtifactSpec | null
  readonly receiveIntent: ReceiveIntent | null
  readonly plan: MaterializationPlan | null
  readonly lifecycle: ReceiveLifecycleState | null
  readonly lifecyclePresentation: ReceiveLifecyclePresentation | null
  readonly expiresAt: number | null
  readonly workspaceUsage: WorkspaceUsage | null
  readonly activeControls: readonly V2ActiveReceiveControl[]
  readonly transferResultPresentation: TransferResultPresentation | null
}

export const EMPTY_V2_OUTPUT_PRESENTATION: V2OutputPresentationSnapshot = Object.freeze({
  projection: null,
  offers: null,
  offerPresentation: null,
  chosenAction: null,
  chosenArtifactKind: null,
  chosenArtifact: null,
  receiveIntent: null,
  plan: null,
  lifecycle: null,
  lifecyclePresentation: null,
  expiresAt: null,
  workspaceUsage: null,
  activeControls: Object.freeze([]),
  transferResultPresentation: null,
})

export type V2OutputTraceEvent =
  | Readonly<{
      name: 'authority_transition'
      transition: 'offers_computed'
      projectionEpoch: ProjectionEpoch
      shapeProof: OfferComputedDecision['shape_proof']
      offeredArtifactKinds: OfferComputedDecision['offered_artifact_kinds']
      offeredPlanKinds: OfferComputedDecision['offered_plan_kinds']
      primaryArtifactKind: OfferComputedDecision['primary_artifact_kind']
    }>
  | Readonly<{
      name: 'authority_transition'
      transition: 'offers_disabled'
      projectionEpoch: ProjectionEpoch
      shapeProof: OfferDisabledDecision['shape_proof']
      reason: OfferDisabledDecision['offer_unavailable_reason']
      hardLimitClass?: NonNullable<OfferDisabledDecision['hard_limit_class']>
    }>
  | Readonly<{
      name: 'authority_transition'
      transition: 'stale_event_dropped'
      currentProjectionEpoch: ProjectionEpoch
      staleProjectionEpoch: ProjectionEpoch
      eventClass: 'capability_result' | 'artifact_action' | 'authority_result'
    }>

function offerDecisionTraceEvent(
  decision: OfferComputedDecision | OfferDisabledDecision,
): V2OutputTraceEvent {
  if (decision.name === 'receive.offer.computed') {
    return Object.freeze({
      name: 'authority_transition',
      transition: 'offers_computed',
      projectionEpoch: decision.projection_epoch,
      shapeProof: decision.shape_proof,
      offeredArtifactKinds: decision.offered_artifact_kinds,
      offeredPlanKinds: decision.offered_plan_kinds,
      primaryArtifactKind: decision.primary_artifact_kind,
    })
  }
  return Object.freeze({
    name: 'authority_transition',
    transition: 'offers_disabled',
    projectionEpoch: decision.projection_epoch,
    shapeProof: decision.shape_proof,
    reason: decision.offer_unavailable_reason,
    ...(decision.hard_limit_class === undefined
      ? {}
      : { hardLimitClass: decision.hard_limit_class }),
  })
}

export type ArtifactOfferPlanner = (
  projection: SelectionProjectionState['projection'],
  discovery: SelectionProjectionState['discovery'],
  environment: EnvironmentOffers,
) => Promise<ArtifactOffers>

export type ArtifactAuthorityStarter<Authority> = (
  action: ArtifactAction,
) => Authority | PromiseLike<Authority>

export type ArtifactActivationResult<Authority> =
  | Readonly<{ kind: 'unavailable' }>
  | Readonly<{
      kind: 'acquired'
      projectionEpoch: ProjectionEpoch
      action: ArtifactAction
      authority: Authority
    }>
  | Readonly<{
      kind: 'stale'
      projectionEpoch: ProjectionEpoch
      action: ArtifactAction
    }>

export type ProjectionPresentationResult =
  | Readonly<{ kind: 'applied'; offers: ArtifactOffers }>
  | Readonly<{ kind: 'stale'; projectionEpoch: ProjectionEpoch }>

export type RetryConfirmationResult =
  | Readonly<{ kind: 'unavailable' }>
  | Readonly<{ kind: 'completed'; projectionEpoch: ProjectionEpoch }>
  | Readonly<{ kind: 'stale'; projectionEpoch: ProjectionEpoch }>

export type TransferResultProjection = Pick<
  TransferJobResult,
  'worker' | 'intent' | 'transferJobId'
>

export interface V2OutputPresentationControllerOptions<Authority> {
  readonly planner?: ArtifactOfferPlanner
  readonly releaseStaleAuthority?: (authority: Authority) => void | PromiseLike<void>
  readonly trace?: DomainTraceSource<V2OutputTraceEvent>
}

interface PendingActivation {
  readonly boundary: number
  readonly epoch: ProjectionEpoch
  readonly action: ArtifactAction
}

/**
 * This controller never owns an authority callback. Requiring it at activation
 * keeps output authority reachable only from the final rendered artifact click.
 */
export class V2OutputPresentationController<Authority = unknown> {
  readonly #planner: ArtifactOfferPlanner
  readonly #releaseStaleAuthority: ((authority: Authority) => void | PromiseLike<void>) | undefined
  readonly #traceSource: DomainTraceSource<V2OutputTraceEvent> | undefined
  readonly #listeners = new Set<() => void>()
  #snapshot = EMPTY_V2_OUTPUT_PRESENTATION
  #boundary = 0
  #pendingActivation: PendingActivation | undefined

  constructor(options: V2OutputPresentationControllerOptions<Authority> = {}) {
    this.#planner = options.planner ?? offerArtifacts
    this.#releaseStaleAuthority = options.releaseStaleAuthority
    this.#traceSource = options.trace
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): V2OutputPresentationSnapshot => this.#snapshot

  updateProjection(
    state: SelectionProjectionState,
    environment: EnvironmentOffers,
  ): Promise<ProjectionPresentationResult> {
    const boundary = ++this.#boundary
    const epoch = state.projection.epoch
    const changedEpoch = this.#snapshot.projection?.projection.epoch !== epoch
    this.#pendingActivation = undefined
    this.#publish(Object.freeze({
      ...(changedEpoch ? EMPTY_V2_OUTPUT_PRESENTATION : this.#snapshot),
      projection: state,
      offers: null,
      offerPresentation: null,
      ...(changedEpoch
        ? {
            chosenAction: null,
            chosenArtifactKind: null,
            chosenArtifact: null,
            receiveIntent: null,
            plan: null,
            lifecycle: null,
            lifecyclePresentation: null,
            expiresAt: null,
            workspaceUsage: null,
            activeControls: Object.freeze([]),
            transferResultPresentation: null,
          }
        : {}),
    }))

    return this.#planner(state.projection, state.discovery, environment).then((offers) => {
      const currentEpoch = this.#snapshot.projection?.projection.epoch
      if (boundary !== this.#boundary || currentEpoch !== epoch) {
        this.#traceStale(epoch, 'capability-result')
        return Object.freeze({ kind: 'stale', projectionEpoch: epoch })
      }
      if (offers.projectionEpoch !== epoch) {
        throw new TypeError('artifact offers do not belong to the current projection epoch')
      }
      this.#publish(Object.freeze({
        ...this.#snapshot,
        offers,
        offerPresentation: presentArtifactOffers(offers),
        chosenAction: null,
        chosenArtifactKind: null,
        chosenArtifact: null,
      }))
      this.#emitTrace(() => offerDecisionTraceEvent(offers.decision))
      return Object.freeze({ kind: 'applied', offers })
    })
  }

  /** The authority starter is invoked before this method returns to the click handler. */
  activateArtifact(
    operation: ArtifactOperation,
    startAuthority: ArtifactAuthorityStarter<Authority>,
  ): Promise<ArtifactActivationResult<Authority>> {
    const offers = this.#snapshot.offers
    if (offers?.kind !== 'artifact-actions' || this.#pendingActivation !== undefined ||
        this.#snapshot.chosenAction !== null) {
      return Promise.resolve(Object.freeze({ kind: 'unavailable' }))
    }
    const action = [offers.primary, ...offers.alternatives]
      .find((candidate) => candidate.operation === operation)
    if (action === undefined) return Promise.resolve(Object.freeze({ kind: 'unavailable' }))

    const boundary = this.#boundary
    const epoch = offers.projectionEpoch
    let started: Authority | PromiseLike<Authority>
    try {
      // No state publication or asynchronous work may move ahead of this call.
      started = startAuthority(action)
    } catch (error) {
      return Promise.reject(error)
    }
    const pending = Object.freeze({ boundary, epoch, action })
    this.#pendingActivation = pending
    this.#publish(Object.freeze({
      ...this.#snapshot,
      chosenAction: action,
      chosenArtifactKind: action.artifactKind,
      chosenArtifact: action.artifact,
    }))

    return Promise.resolve(started).then(async (authority): Promise<ArtifactActivationResult<Authority>> => {
      if (this.#pendingActivation === pending) this.#pendingActivation = undefined
      if (!this.#activationIsCurrent(pending)) {
        await this.#releaseStaleAuthority?.(authority)
        this.#traceStale(epoch, 'authority-result')
        return Object.freeze({ kind: 'stale', projectionEpoch: epoch, action })
      }
      return Object.freeze({ kind: 'acquired', projectionEpoch: epoch, action, authority })
    }, (error: unknown) => {
      const remainsCurrent = this.#activationIsCurrent(pending)
      if (this.#pendingActivation === pending) this.#pendingActivation = undefined
      if (remainsCurrent) {
        // Cancellation before intent freeze returns to the same offered action.
        this.#publish(Object.freeze({
          ...this.#snapshot,
          chosenAction: null,
          chosenArtifactKind: null,
          chosenArtifact: null,
        }))
      }
      throw error
    })
  }

  retryConfirmation(
    startRetry: (epoch: ProjectionEpoch) => void | PromiseLike<void>,
  ): Promise<RetryConfirmationResult> {
    const offers = this.#snapshot.offers
    if (offers?.kind !== 'retry-confirmation') {
      return Promise.resolve(Object.freeze({ kind: 'unavailable' }))
    }
    const boundary = this.#boundary
    const epoch = offers.projectionEpoch
    let started: void | PromiseLike<void>
    try {
      started = startRetry(epoch)
    } catch (error) {
      return Promise.reject(error)
    }
    return Promise.resolve(started).then(() => {
      if (boundary !== this.#boundary || this.#snapshot.projection?.projection.epoch !== epoch) {
        this.#traceStale(epoch, 'artifact-action')
        return Object.freeze({ kind: 'stale', projectionEpoch: epoch })
      }
      return Object.freeze({ kind: 'completed', projectionEpoch: epoch })
    })
  }

  adoptReceiveIntent(
    projectionEpoch: ProjectionEpoch,
    intent: ReceiveIntent,
    lifecycle?: ReceiveLifecycleState,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
  ): boolean {
    const action = this.#snapshot.chosenAction
    if (this.#snapshot.projection?.projection.epoch !== projectionEpoch ||
        action?.projectionEpoch !== projectionEpoch) {
      this.#traceStale(projectionEpoch, 'artifact-action')
      return false
    }
    if (action.artifactKind !== intent.artifact.kind || action.plan.kind !== intent.plan.kind ||
        (action.artifact !== null && action.artifact.digest !== intent.artifact.digest)) {
      throw new TypeError('bound receive intent does not match the chosen artifact action')
    }
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      chosenArtifactKind: intent.artifact.kind,
      chosenArtifact: intent.artifact,
      receiveIntent: intent,
      plan: intent.plan,
      activeControls: Object.freeze([...activeControls]),
    }, lifecycle ?? null, nowMilliseconds, workspaceUsage)
    return true
  }

  adoptRetainedReceiveIntent(
    intent: ReceiveIntent,
    lifecycle: ReceiveLifecycleState,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
  ): void {
    if (lifecycle.operationId !== intent.operationId ||
        lifecycle.receiveIntentDigest !== intent.digest) {
      throw new TypeError('retained lifecycle does not belong to its validated receive intent')
    }
    this.#boundary += 1
    this.#pendingActivation = undefined
    this.#publishLifecycleSnapshot({
      ...EMPTY_V2_OUTPUT_PRESENTATION,
      chosenArtifactKind: intent.artifact.kind,
      chosenArtifact: intent.artifact,
      receiveIntent: intent,
      plan: intent.plan,
      activeControls: Object.freeze([...activeControls]),
    }, lifecycle, nowMilliseconds, workspaceUsage)
  }

  updateLifecycle(
    state: ReceiveLifecycleState,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
  ): boolean {
    const intent = this.#snapshot.receiveIntent
    if (intent === null || state.operationId !== intent.operationId ||
        state.receiveIntentDigest !== intent.digest) return false
    const current = this.#snapshot.lifecycle
    if (current !== null && state.generation <= current.generation) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      activeControls: Object.freeze([...activeControls]),
    }, state, nowMilliseconds, workspaceUsage)
    return true
  }

  updateActiveControls(controls: readonly V2ActiveReceiveControl[]): boolean {
    const lifecycle = this.#snapshot.lifecycle
    if (lifecycle === null) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      activeControls: Object.freeze([...controls]),
    }, lifecycle, Date.now(), this.#snapshot.workspaceUsage)
    return true
  }

  adoptTransferResult(
    projectionEpoch: ProjectionEpoch | null,
    result: TransferResultProjection,
  ): boolean {
    const intent = this.#snapshot.receiveIntent
    const currentProjectionEpoch = this.#snapshot.projection?.projection.epoch ?? null
    if (currentProjectionEpoch !== projectionEpoch || intent === null ||
        result.intent.operationId !== intent.operationId || result.intent.digest !== intent.digest) {
      if (projectionEpoch !== null) this.#traceStale(projectionEpoch, 'artifact-action')
      return false
    }
    this.#publish(Object.freeze({
      ...this.#snapshot,
      transferResultPresentation: presentTransferResult(result.worker),
    }))
    return true
  }

  /** Revalidates the result at the controller's post-promise delivery boundary. */
  acquiredAuthorityIsCurrent(
    result: Extract<ArtifactActivationResult<Authority>, { kind: 'acquired' }>,
  ): boolean {
    const offers = this.#snapshot.offers
    const current = this.#snapshot.projection?.projection.epoch === result.projectionEpoch &&
      offers?.kind === 'artifact-actions' &&
      offers.projectionEpoch === result.projectionEpoch &&
      this.#snapshot.chosenAction === result.action
    if (!current) this.#traceStale(result.projectionEpoch, 'authority-result')
    return current
  }

  invalidate(): void {
    this.#boundary += 1
    this.#pendingActivation = undefined
    this.#publish(EMPTY_V2_OUTPUT_PRESENTATION)
  }

  close(): void {
    this.invalidate()
    this.#listeners.clear()
  }

  #publishLifecycleSnapshot(
    base: V2OutputPresentationSnapshot,
    lifecycle: ReceiveLifecycleState | null,
    nowMilliseconds: number,
    workspaceUsage: WorkspaceUsage | null | undefined,
  ): void {
    const artifact = base.chosenArtifact
    const plan = base.plan
    if (lifecycle === null) {
      this.#publish(Object.freeze({
        ...base,
        lifecycle: null,
        lifecyclePresentation: null,
        expiresAt: null,
        workspaceUsage: null,
        activeControls: Object.freeze([]),
      }))
      return
    }
    if (artifact === null || plan === null) {
      throw new TypeError('lifecycle presentation requires a bound artifact and plan')
    }
    const lifecyclePresentation = presentReceiveLifecycle({
      state: lifecycle,
      artifact,
      planKind: plan.kind,
      nowMilliseconds,
      ...(workspaceUsage === undefined ? {} : { workspaceUsage }),
      activeControls: base.activeControls,
    })
    this.#publish(Object.freeze({
      ...base,
      lifecycle,
      lifecyclePresentation,
      expiresAt: lifecycleDeadline(lifecycle) ??
        (lifecycle.kind === 'expired' ? lifecycle.expiresAt : null),
      workspaceUsage: lifecyclePresentation.usage === null
        ? null
        : Object.freeze({
            ownedBytes: lifecyclePresentation.usage.ownedBytes,
            ...(lifecyclePresentation.usage.maximumBytes === undefined
              ? {}
              : { maximumBytes: lifecyclePresentation.usage.maximumBytes }),
          }),
    }))
  }

  #activationIsCurrent(pending: PendingActivation): boolean {
    const offers = this.#snapshot.offers
    return pending.boundary === this.#boundary &&
      this.#snapshot.projection?.projection.epoch === pending.epoch &&
      offers?.kind === 'artifact-actions' &&
      offers.projectionEpoch === pending.epoch &&
      this.#snapshot.chosenAction === pending.action
  }

  #traceStale(
    staleEpoch: ProjectionEpoch,
    eventClass: 'capability-result' | 'artifact-action' | 'authority-result',
  ): void {
    const currentEpoch = this.#snapshot.projection?.projection.epoch
    if (currentEpoch === undefined || currentEpoch === staleEpoch) return
    this.#emitTrace(() => Object.freeze({
      name: 'authority_transition',
      transition: 'stale_event_dropped',
      currentProjectionEpoch: currentEpoch,
      staleProjectionEpoch: staleEpoch,
      eventClass: staleTraceEventClass(eventClass),
    }))
  }

  #emitTrace(createEvent: () => V2OutputTraceEvent): void {
    const observer = this.#traceSource?.current
    if (observer === undefined) return
    try {
      observer(createEvent())
    } catch {
      // Observers cannot change artifact choice, authority ownership, or epoch fencing.
    }
  }

  #publish(snapshot: V2OutputPresentationSnapshot): void {
    this.#snapshot = snapshot
    for (const listener of this.#listeners) listener()
  }
}

function staleTraceEventClass(
  eventClass: 'capability-result' | 'artifact-action' | 'authority-result',
): 'capability_result' | 'artifact_action' | 'authority_result' {
  switch (eventClass) {
    case 'capability-result':
      return 'capability_result'
    case 'artifact-action':
      return 'artifact_action'
    case 'authority-result':
      return 'authority_result'
  }
}
