import {
  materializationRouteIdentity,
  sameArtifactChoiceSemantics,
  sameMaterializationRouteIdentity,
  type ArtifactOperation,
} from '../../output/planning'
import type { SelectionSpec } from '../../transfer/intent'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type {
  V2ArtifactPresentationAuthority,
  V2BoundReceiveOperation,
} from '../v2-receive-runtime'
import { presentationSourceOutcome } from '../v2-receive-runtime'
import type {
  ActiveReceiveAdoption,
  ActiveReceiveCoordinator,
} from './active-receive'
import type {
  V2AuthorityActivationSnapshot,
  V2AuthorityActivationTerminalOutcome,
} from './activation-model'
import { V2ActivationStateContractError } from './activation-model'
import {
  StaleReceiveBoundaryError,
} from './contracts'
import {
  AuthorityCommitTransaction,
  boundOperationCleanup,
  ownedEffectsCleanup,
  type ActivationCleanupOwner,
  type AuthorityCommitOutcome,
  type ReceiveIntentBinder,
} from './authority-commit'
import {
  AuthorityPlanningPipeline,
  planningSubject,
  type AuthorityPlanningRequest,
  type AuthorityPlanningResult,
  type AuthorityPlanningSubject,
  type V2AuthorityProjectionPublication,
} from './authority-planning'
import {
  cleanupRequiredSnapshot,
  closeActivationAttempt,
  createActivationId,
  createAuthorityActivationRecord,
  observationMatchesActivation,
  projectLiveActivation,
  releaseActivationAuthority,
  runActivationCleanup,
  terminalAuthoritySnapshot,
  traceAuthorityTransition,
  type AuthorityActivationRecord,
  type AuthorityActivationOptions,
  type AuthorityAttemptExclusion,
  type AuthorityTraceDetail,
} from './authority-lifecycle'
import type { V2ActiveProjection } from './projection-observation'

export type { AuthorityActivationOptions } from './authority-lifecycle'

/**
 * Owns activation from the trusted click through the commit linearization point.
 * Projection/session objects are replaceable observations; only authenticated
 * share, selection, semantic choice, and installed route bind local authority.
 */
export class V2AuthorityActivationCoordinator {
  readonly #options: AuthorityActivationOptions
  readonly #planning: AuthorityPlanningPipeline
  readonly #binder: ReceiveIntentBinder | undefined
  readonly #createActivationId: () => string
  readonly #listeners = new Set<() => void>()
  #snapshot: V2AuthorityActivationSnapshot = Object.freeze({ kind: 'inactive' })
  #activation: AuthorityActivationRecord | undefined
  #joinSuspended = false
  #joinReplacement: V2JoinedBrowserShare | undefined

  constructor(options: AuthorityActivationOptions) {
    this.#options = options
    this.#binder = options.binder
    this.#createActivationId = options.createActivationId ?? createActivationId
    this.#planning = new AuthorityPlanningPipeline({
      currentProjection: options.currentProjection,
      offersApplied: (request, offers) => this.#offersApplied(request, offers),
      planningFailed: error => this.#planningFailed(error),
      reconciliationApplied: (subject, request, outcome) =>
        this.#applyReconciliation(subject, request, outcome),
      reconciliationFailed: (subject, error) => this.#reconciliationFailed(subject, error),
      reconciliationEvidenceAdvanced: subject => this.#planningEvidenceAdvanced(subject),
      ...(options.planner === undefined ? {} : { planner: options.planner }),
      ...(options.reconciler === undefined ? {} : { reconciler: options.reconciler }),
    })
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): V2AuthorityActivationSnapshot => this.#snapshot

  get pending(): boolean {
    const activation = this.#activation
    return activation !== undefined && (
      activation.terminal === undefined ||
      activation.commitAttempt !== undefined ||
      activation.cleanup !== undefined
    )
  }

  observeProjection(active: V2ActiveProjection): void {
    const activation = this.#activation
    if (activation !== undefined && activation.terminal === undefined) {
      this.#beginObservationReplacement(activation, new StaleReceiveBoundaryError(), false)
    }
    const request = this.#planning.observe(
      active,
      activation === undefined || activation.terminal !== undefined
        ? undefined
        : planningSubject(activation),
    )
    if (activation !== undefined && activation.terminal === undefined) {
      if (active.selection.shareInstance !== activation.authenticatedShareInstanceId) {
        this.#invalidate(activation, 'authenticated-share-instance-changed')
      } else if (active.selection.digest !== activation.selectionDigest &&
          active.epoch !== activation.projectionEpoch) {
        this.#invalidate(activation, 'selection-changed')
      } else {
        activation.joined = active.joined
        activation.observationRevision = request.revision
        activation.protocolSessionId = active.protocolSessionId
        activation.projectionEpoch = active.epoch
        activation.observationReplacementPending = false
        delete activation.resolution
        delete activation.retryReason
        if (this.#joinReplacement === active.joined) {
          this.#joinReplacement = undefined
          this.#joinSuspended = false
        }
        this.#publishLiveSnapshot(activation)
      }
    }

  }

  /**
   * Opens the no-projection interval before the projection owner clears its old
   * observation. Pre-fence route work is cancelled, while fenced ownership stays
   * with this coordinator until a replacement can adopt or settle it.
   */
  startObservationReplacement(reason: unknown): void {
    const activation = this.#activation
    if (activation === undefined || activation.terminal !== undefined) return
    this.#beginObservationReplacement(activation, reason)
  }

  choose(operation: ArtifactOperation): boolean {
    const current = this.#options.currentProjection()
    const planned = this.#planning.latestOffers
    if (current === undefined || planned === undefined || planned.request.active !== current ||
        planned.request.state !== current.state || !this.#planning.isCurrent(planned.request) ||
        this.#options.choiceBlocked() || this.pending ||
        planned.offers.kind !== 'artifact-actions') return false
    const offered = [planned.offers.primary, ...planned.offers.alternatives]
      .find(candidate => candidate.choice.operation === operation)
    if (offered === undefined) return false

    const attempt = this.#options.observability.open('authority_activation')
    let activationId: string
    try {
      activationId = this.#createActivationId()
    } catch (error) {
      this.#options.observability.fail(attempt, 'projection_authority', error, 'authority_activation')
      attempt.close()
      this.#options.publishActionError(error)
      return false
    }
    const record = createAuthorityActivationRecord({
      activationId,
      authenticatedShareInstanceId: current.selection.shareInstance,
      selectionDigest: current.selection.digest,
      choice: offered.choice,
      installedRoute: materializationRouteIdentity(offered.route),
      attempt,
      joined: current.joined,
      observationRevision: planned.request.revision,
      protocolSessionId: current.protocolSessionId,
      projectionEpoch: current.epoch,
    })
    this.#activation = record
    this.#trace(record, { transition: 'activation_started' })

    let authority: V2ArtifactPresentationAuthority
    try {
      // Installing the record first closes reentrant and repeated-click picker races.
      authority = this.#options.receive.startArtifactAuthority(offered, attempt.outputFailures)
      record.authority = authority
    } catch (error) {
      this.#handlePresentationSourceFailure(record, error)
      return true
    }

    this.#publishLiveSnapshot(record)
    this.#observeAuthority(record, authority)
    this.#planning.reconcile(planned.request, planningSubject(record))
    return true
  }

  retry(): boolean {
    const activation = this.#activation
    if (activation?.cleanup !== undefined) {
      if (activation.cleanup.running) return false
      this.#runCleanup(activation, activation.cleanup)
      return true
    }
    const active = this.#options.currentProjection()
    if (active === undefined) return false
    if (activation !== undefined && activation.terminal === undefined) {
      if (activation.retryReason === undefined) return false
    } else if (this.#planning.latestOffers?.offers.kind !== 'retry-confirmation') {
      return false
    }
    this.#options.retryProjection(active)
    return true
  }

  suspendForJoin(): void {
    this.#joinSuspended = true
    this.#joinReplacement = undefined
    const activation = this.#activation
    if (activation !== undefined && activation.terminal === undefined) {
      this.#beginObservationReplacement(activation, new StaleReceiveBoundaryError())
    }
  }

  completeJoin(joined: V2JoinedBrowserShare, selection: SelectionSpec): void {
    const activation = this.#activation
    if (activation === undefined || activation.terminal !== undefined) {
      this.#joinSuspended = false
      return
    }
    if (selection.shareInstance !== activation.authenticatedShareInstanceId) {
      this.#joinSuspended = false
      this.#invalidate(activation, 'authenticated-share-instance-changed')
      return
    }
    if (selection.digest !== activation.selectionDigest) {
      this.#joinSuspended = false
      this.#invalidate(activation, 'selection-changed')
      return
    }
    activation.joined = joined
    this.#joinReplacement = joined
  }

  cancelJoin(): void {
    this.#joinSuspended = false
    this.#joinReplacement = undefined
    const activation = this.#activation
    if (activation !== undefined) this.#maybeCommit(activation)
  }

  invalidate(reason: unknown, invalidation: 'caller-cancelled' | 'selection-changed'): void {
    this.#planning.invalidated()
    const activation = this.#activation
    if (activation !== undefined && activation.terminal === undefined) {
      if (invalidation === 'caller-cancelled') activation.controller.abort(reason)
      this.#invalidate(activation, invalidation)
    }
  }

  close(reason: unknown): void {
    this.invalidate(reason, 'caller-cancelled')
    this.#listeners.clear()
  }

  #observeAuthority(
    record: AuthorityActivationRecord,
    authority: V2ArtifactPresentationAuthority,
  ): void {
    Promise.resolve(authority.ready).then(() => {
      if (this.#activation !== record || record.terminal !== undefined) return
      record.authorityReady = true
      this.#publishLiveSnapshot(record)
      this.#maybeCommit(record)
    }, (error: unknown) => {
      if (this.#activation !== record || record.terminal !== undefined) return
      this.#handlePresentationSourceFailure(record, error)
    }).catch(() => undefined)
  }

  #offersApplied(
    request: AuthorityPlanningRequest,
    offers: V2AuthorityProjectionPublication['offers'],
  ): void {
    this.#options.publishProjection(Object.freeze({
      observationRevision: request.revision,
      state: request.state,
      offers,
    }))
    const activation = this.#activation
    if (activation !== undefined) {
      this.#maybeCommit(activation)
      this.#maybeAdoptBoundResult(activation)
    }
  }

  #applyReconciliation(
    subject: AuthorityPlanningSubject,
    request: AuthorityPlanningRequest,
    outcome: AuthorityPlanningResult,
  ): void {
    const record = subject.key as AuthorityActivationRecord
    if (this.#activation !== record || record.terminal !== undefined) return
    record.observationRevision = request.revision
    record.protocolSessionId = request.active.protocolSessionId
    record.projectionEpoch = request.active.epoch
    delete record.resolution
    delete record.retryReason
    switch (outcome.kind) {
      case 'waiting':
        this.#publishLiveSnapshot(record)
        this.#maybeAdoptBoundResult(record)
        return
      case 'retry-required':
        record.retryReason = outcome.reason
        this.#trace(record, {
          transition: 'retry_required',
          retryableDiscoveryReason: outcome.reason,
        })
        this.#publishLiveSnapshot(record)
        this.#maybeAdoptBoundResult(record)
        return
      case 'invalidated':
        this.#invalidate(record, outcome.reason)
        return
      case 'resolved':
        record.resolution = outcome.action
        this.#trace(record, { transition: 'artifact_resolved' })
        this.#publishLiveSnapshot(record)
        this.#maybeCommit(record)
        this.#maybeAdoptBoundResult(record)
    }
  }

  #planningFailed(error: unknown): void {
    const activation = this.#activation
    if (activation !== undefined && activation.terminal === undefined) {
      this.#fail(activation, error)
      return
    }
    this.#options.publishActionError(error)
  }

  #reconciliationFailed(subject: AuthorityPlanningSubject, error: unknown): void {
    const record = subject.key as AuthorityActivationRecord
    if (this.#activation !== record || record.terminal !== undefined) return
    this.#fail(record, error)
  }

  #planningEvidenceAdvanced(subject: AuthorityPlanningSubject): void {
    const record = subject.key as AuthorityActivationRecord
    if (this.#activation !== record) return
    this.#maybeCommit(record)
    this.#maybeAdoptBoundResult(record)
  }

  #maybeCommit(record: AuthorityActivationRecord): void {
    if (this.#activation !== record || record.terminal !== undefined ||
        record.commitAttempt !== undefined || record.cleanup !== undefined ||
        record.observationReplacementPending ||
        this.#joinSuspended || !record.authorityReady || record.resolution === undefined ||
        record.authority === undefined || !this.#planning.readyForCommit(
          planningSubject(record),
          record.projectionEpoch,
          record.observationRevision,
        )) return
    const attempt = new AuthorityCommitTransaction({
      action: record.resolution,
      observationRevision: record.observationRevision,
      authority: record.authority,
      assertFinalFence: transaction => this.#assertFinalFence(record, transaction),
      ...(this.#binder === undefined ? {} : { binder: this.#binder }),
    })
    record.commitAttempt = attempt
    this.#publishLiveSnapshot(record)
    this.#trace(record, { transition: 'commit_started' })
    attempt.run().then(
      result => this.#commitCompleted(record, attempt, result),
      error => this.#commitRejected(record, attempt, error),
    ).catch(() => undefined)
  }

  #assertFinalFence(
    record: AuthorityActivationRecord,
    attempt: AuthorityCommitTransaction,
  ): SelectionSpec {
    const active = this.#options.currentProjection()
    const joined = this.#options.currentJoinedShare()
    if (this.#activation !== record || record.terminal !== undefined ||
        record.commitAttempt !== attempt || record.observationReplacementPending ||
        this.#joinSuspended || !observationMatchesActivation(record, active, joined) ||
        !this.#planning.readyForCommit(
          planningSubject(record),
          record.projectionEpoch,
          record.observationRevision,
        ) ||
        record.resolution !== attempt.action ||
        !sameArtifactChoiceSemantics(record.choice, attempt.action.choice) ||
        !sameMaterializationRouteIdentity(
          record.installedRoute,
          materializationRouteIdentity(attempt.action.route),
        )) {
      throw new StaleReceiveBoundaryError()
    }
    return active.selection
  }

  #commitCompleted(
    record: AuthorityActivationRecord,
    attempt: AuthorityCommitTransaction,
    result: AuthorityCommitOutcome,
  ): void {
    if (record.commitAttempt !== attempt) return
    if (result.kind === 'retryable-precut') {
      this.#trace(record, {
        transition: 'commit_pre_cut_retry',
        ...(result.receiverOperationId === undefined
          ? {}
          : { receiverOperationId: result.receiverOperationId }),
      })
      delete record.commitAttempt
      if (record.terminal !== undefined) {
        releaseActivationAuthority(
          record,
          this.#options.observability,
          new StaleReceiveBoundaryError(),
        ).finally(() => {
          this.#trace(record, { transition: 'cleanup_completed' })
          closeActivationAttempt(record, this.#options.observability)
        }).catch(() => undefined)
        return
      }
      if (!record.observationReplacementPending &&
          record.observationRevision <= attempt.observationRevision) {
        this.#fail(record, new V2ActivationStateContractError(
          'route requested a pre-cut retry without a replacement observation',
        ))
        return
      }
      this.#publishLiveSnapshot(record)
      this.#maybeCommit(record)
      return
    }
    if (result.kind === 'owned-effects') {
      this.#trace(record, {
        transition: 'commit_owned_effects',
        receiverOperationId: result.authority.intent.operationId,
      })
      delete record.commitAttempt
      record.released = true
      if (this.#activation === record && record.terminal === undefined) {
        this.#reportFailure(record, result.cause)
      }
      this.#beginCleanup(record, ownedEffectsCleanup(
        result,
        record.terminal ?? Object.freeze({
          kind: 'owned-effects-settled',
          operationId: result.authority.intent.operationId,
        }),
      ))
      return
    }
    const runtime = result.operation
    this.#trace(record, {
      transition: 'commit_bound_operation',
      receiverOperationId: runtime.intent.operationId,
    })
    this.#maybeAdoptBoundResult(record)
  }

  #maybeAdoptBoundResult(record: AuthorityActivationRecord): void {
    const attempt = record.commitAttempt
    const result = attempt?.outcome
    if (attempt === undefined || result?.kind !== 'bound-operation' ||
        record.cleanup !== undefined) return
    const runtime = result.operation
    const intent = result.intent
    if (record.terminal !== undefined) {
      this.#beginBoundRuntimeCleanup(record, attempt, runtime, new StaleReceiveBoundaryError())
      return
    }
    const active = this.#currentObservation(record)
    const resolution = record.resolution
    if (active === undefined || resolution === undefined) return
    if (!sameArtifactChoiceSemantics(record.choice, resolution.choice) ||
        !sameMaterializationRouteIdentity(
          record.installedRoute,
          materializationRouteIdentity(resolution.route),
        ) || resolution.artifact.digest !== attempt.action.artifact.digest ||
        intent.artifact.digest !== resolution.artifact.digest) {
      const error = new StaleReceiveBoundaryError()
      this.#invalidate(record, 'artifact-shape-incompatible')
      this.#beginBoundRuntimeCleanup(record, attempt, runtime, error)
      return
    }
    const adoption: ActiveReceiveAdoption = {
      joined: record.joined,
      selection: active.frozenSelection,
      runtime,
      ...(runtime.repairProjection === undefined
        ? {}
        : { repairProjection: runtime.repairProjection }),
    }
    let prepared: ReturnType<ActiveReceiveCoordinator['prepareAdoption']>
    try {
      prepared = this.#options.activeReceive.prepareAdoption(adoption)
      if (!this.#options.adoptReceiveIntent(record.choice, intent, runtime, prepared.commit)) {
        throw new StaleReceiveBoundaryError()
      }
    } catch (error) {
      if (record.terminal === undefined) this.#reportFailure(record, error)
      this.#beginBoundRuntimeCleanup(record, attempt, runtime, error)
      return
    }
    prepared.start()
    delete record.commitAttempt
    record.released = true
    this.#terminalWithoutRelease(record, Object.freeze({
      kind: 'bound-operation',
      operationId: intent.operationId,
    }), 'success')
    active.controller.abort(new DOMException('Receive intent is frozen', 'AbortError'))
  }

  #commitRejected(
    record: AuthorityActivationRecord,
    attempt: AuthorityCommitTransaction,
    error: unknown,
  ): void {
    if (record.commitAttempt !== attempt) return
    delete record.commitAttempt
    if (this.#activation !== record || record.terminal !== undefined) {
      releaseActivationAuthority(record, this.#options.observability, error).finally(() => {
        this.#trace(record, { transition: 'cleanup_completed' })
        closeActivationAttempt(record, this.#options.observability)
      }).catch(() => undefined)
      return
    }
    this.#fail(record, error)
  }

  #beginBoundRuntimeCleanup(
    record: AuthorityActivationRecord,
    attempt: AuthorityCommitTransaction,
    runtime: V2BoundReceiveOperation,
    reason: unknown,
  ): void {
    if (record.commitAttempt === attempt) delete record.commitAttempt
    record.released = true
    this.#beginCleanup(
      record,
      boundOperationCleanup(
        runtime,
        reason,
        record.terminal ?? Object.freeze({ kind: 'failed' }),
      ),
    )
  }

  #beginCleanup(
    record: AuthorityActivationRecord,
    cleanup: ActivationCleanupOwner,
  ): void {
    if (record.cleanup !== undefined) return
    record.cleanup = cleanup
    this.#runCleanup(record, cleanup)
  }

  #runCleanup(record: AuthorityActivationRecord, cleanup: ActivationCleanupOwner): void {
    if (record.cleanup !== cleanup) return
    runActivationCleanup(
      record,
      cleanup,
      this.#options.observability,
      this.#options.refreshRetainedInventory,
      {
        completed: () => {
          if (record.cleanup !== cleanup) return
          delete record.cleanup
          this.#trace(record, {
            transition: 'cleanup_completed',
            receiverOperationId: cleanup.operationId,
          })
          if (record.terminal === undefined) {
            this.#markTerminal(record, cleanup.outcome, undefined)
          } else {
            this.#publishSnapshot(terminalAuthoritySnapshot(record))
          }
          closeActivationAttempt(record, this.#options.observability)
        },
        pending: failedStage => {
          if (record.cleanup === cleanup) {
            this.#publishSnapshot(cleanupRequiredSnapshot(record, failedStage))
          }
        },
      },
    )
  }

  #invalidate(
    record: AuthorityActivationRecord,
    reason: Extract<V2AuthorityActivationTerminalOutcome, { kind: 'invalidated' }>['reason'],
  ): void {
    this.#trace(record, { transition: 'semantic_invalidated', invalidationReason: reason })
    this.#terminal(
      record,
      Object.freeze({ kind: 'invalidated', reason }),
      reason === 'caller-cancelled' ? 'cancelled' : 'authority_invalidated',
      new StaleReceiveBoundaryError(),
    )
  }

  #fail(record: AuthorityActivationRecord, error: unknown, release = true): void {
    if (this.#activation !== record || record.terminal !== undefined) return
    this.#reportFailure(record, error)
    const outcome = Object.freeze({ kind: 'failed' as const })
    if (release) {
      this.#terminal(record, outcome, undefined, error)
    } else {
      this.#terminalWithoutRelease(record, outcome, undefined)
    }
  }

  #reportFailure(record: AuthorityActivationRecord, error: unknown): void {
    if (record.failureReported) return
    record.failureReported = true
    this.#options.observability.fail(
      record.attempt,
      'projection_authority',
      error,
      'authority_activation',
    )
    this.#options.publishActionError(error)
  }

  #handlePresentationSourceFailure(record: AuthorityActivationRecord, error: unknown): void {
    if (presentationSourceOutcome(error) === 'picker_refused') {
      this.#terminal(record, Object.freeze({ kind: 'picker-refused' }), 'picker_refused', error)
      return
    }
    this.#fail(record, error)
  }

  #terminal(
    record: AuthorityActivationRecord,
    outcome: V2AuthorityActivationTerminalOutcome,
    exclusion: AuthorityAttemptExclusion | undefined,
    releaseReason: unknown,
  ): void {
    if (!this.#markTerminal(record, outcome, exclusion)) return
    record.controller.abort(releaseReason)
    const attempt = record.commitAttempt
    if (attempt !== undefined) {
      attempt.abortPreFence(releaseReason)
      return
    }
    if (record.cleanup !== undefined) return
    releaseActivationAuthority(record, this.#options.observability, releaseReason).finally(() => {
      this.#trace(record, { transition: 'cleanup_completed' })
      closeActivationAttempt(record, this.#options.observability)
    }).catch(() => undefined)
  }

  #terminalWithoutRelease(
    record: AuthorityActivationRecord,
    outcome: V2AuthorityActivationTerminalOutcome,
    exclusion: AuthorityAttemptExclusion | undefined,
  ): void {
    if (!this.#markTerminal(record, outcome, exclusion)) return
    this.#trace(record, {
      transition: 'cleanup_completed',
      ...('operationId' in outcome ? { receiverOperationId: outcome.operationId } : {}),
    })
    closeActivationAttempt(record, this.#options.observability)
  }

  #markTerminal(
    record: AuthorityActivationRecord,
    outcome: V2AuthorityActivationTerminalOutcome,
    exclusion: AuthorityAttemptExclusion | undefined,
  ): boolean {
    if (this.#activation !== record || record.terminal !== undefined) return false
    record.terminal = outcome
    if (exclusion !== undefined) {
      this.#options.observability.exclude(record.attempt, 'projection_authority', exclusion)
    }
    this.#publishSnapshot(terminalAuthoritySnapshot(record))
    return true
  }

  #publishLiveSnapshot(record: AuthorityActivationRecord): void {
    if (this.#activation !== record || record.terminal !== undefined) return
    if (record.cleanup !== undefined) return
    const projection = projectLiveActivation(record)
    if (projection.waitingFor !== undefined) {
      this.#trace(record, {
        transition: 'prerequisite_waiting',
        waitingFor: projection.waitingFor,
      })
    }
    this.#publishSnapshot(projection.snapshot)
  }

  #publishSnapshot(snapshot: V2AuthorityActivationSnapshot): void {
    this.#snapshot = snapshot
    for (const listener of this.#listeners) listener()
  }

  #beginObservationReplacement(
    record: AuthorityActivationRecord,
    reason: unknown,
    advanceRevision = true,
  ): void {
    if (record.observationReplacementPending) return
    record.observationReplacementPending = true
    this.#planning.beginReplacement(advanceRevision)
    const attempt = record.commitAttempt
    attempt?.abortPreFence(reason)
    this.#publishLiveSnapshot(record)
  }

  #currentObservation(record: AuthorityActivationRecord): V2ActiveProjection | undefined {
    const active = this.#options.currentProjection()
    const joined = this.#options.currentJoinedShare()
    if (record.observationReplacementPending || this.#joinSuspended ||
        !observationMatchesActivation(record, active, joined) ||
        !this.#planning.readyForCommit(
          planningSubject(record),
          record.projectionEpoch,
          record.observationRevision,
        )) return undefined
    return active
  }

  #trace(
    record: AuthorityActivationRecord,
    detail: AuthorityTraceDetail,
  ): void {
    traceAuthorityTransition(this.#options.observability, record, detail)
  }

}
