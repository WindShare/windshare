import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import {
  bindMaterialization,
  type ArtifactOperation,
  type EnvironmentOffers,
} from '../../output/planning'
import {
  validateReceiveIntent,
  type ReceiveIntent,
  type SelectionSpec,
} from '../../transfer/intent'
import type {
  ProjectionEpoch,
  SelectionProjectionState,
} from '../../transfer/projection'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type {
  ArtifactActivationResult,
  V2OutputPresentationController,
} from '../v2-output'
import type {
  V2BoundReceiveOperation,
  V2ReceiveCompositionPort,
  V2StartedArtifactAuthority,
} from '../v2-receive-runtime'
import { presentationSourceOutcome } from '../v2-receive-runtime'
import type { ActiveReceiveCoordinator } from './active-receive'
import {
  ArtifactChoiceInvalidatedError,
  StaleReceiveBoundaryError,
} from './contracts'
import type { V2ControllerObservability } from './controller-observability'
import type { V2PresentationAttempt } from './presentation-attempt'

type AcquiredArtifactActivation = Extract<
  ArtifactActivationResult<V2StartedArtifactAuthority>,
  { kind: 'acquired' }
>

export interface V2ActiveProjection {
  readonly revision: number
  readonly joined: V2JoinedBrowserShare
  readonly selection: SelectionSpec
  readonly frozenSelection: V2FrozenSelectionPolicy
  readonly epoch: ProjectionEpoch
  controller: AbortController
  protocolSessionId: string
  state: SelectionProjectionState
  environment: EnvironmentOffers
}

interface AuthorityActivationOptions {
  readonly outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  readonly receive: V2ReceiveCompositionPort
  readonly activeReceive: ActiveReceiveCoordinator
  readonly observability: V2ControllerObservability
  readonly currentProjection: () => V2ActiveProjection | undefined
  readonly currentJoinedShare: () => V2JoinedBrowserShare | undefined
  readonly choiceBlocked: () => boolean
  readonly restartProjection: (joined: V2JoinedBrowserShare) => void
  readonly publishActionError: (error: unknown) => void
}

/**
 * Owns the complete artifact-authority lifetime. Product state is published at
 * the decision boundary, while release/settlement/detach and scope close remain
 * ordered within this owner without becoming publication prerequisites.
 */
export class V2AuthorityActivationCoordinator {
  readonly #outputs: AuthorityActivationOptions['outputs']
  readonly #receive: AuthorityActivationOptions['receive']
  readonly #activeReceive: AuthorityActivationOptions['activeReceive']
  readonly #observability: AuthorityActivationOptions['observability']
  readonly #currentProjection: AuthorityActivationOptions['currentProjection']
  readonly #currentJoinedShare: AuthorityActivationOptions['currentJoinedShare']
  readonly #choiceBlocked: AuthorityActivationOptions['choiceBlocked']
  readonly #restartProjection: AuthorityActivationOptions['restartProjection']
  readonly #publishActionError: AuthorityActivationOptions['publishActionError']
  #acquisition: AbortController | undefined

  constructor(options: AuthorityActivationOptions) {
    this.#outputs = options.outputs
    this.#receive = options.receive
    this.#activeReceive = options.activeReceive
    this.#observability = options.observability
    this.#currentProjection = options.currentProjection
    this.#currentJoinedShare = options.currentJoinedShare
    this.#choiceBlocked = options.choiceBlocked
    this.#restartProjection = options.restartProjection
    this.#publishActionError = options.publishActionError
  }

  get pending(): boolean {
    return this.#acquisition !== undefined
  }

  abort(reason: unknown): void {
    this.#acquisition?.abort(reason)
    this.#acquisition = undefined
  }

  choose(operation: ArtifactOperation): void {
    const joined = this.#currentJoinedShare()
    const projection = this.#currentProjection()
    if (joined === undefined || projection === undefined || this.#choiceBlocked()) return
    if (projection.protocolSessionId !== joined.protocolSessionId) {
      this.#restartProjection(joined)
      return
    }

    const attempt = this.#observability.open('authority_activation')
    this.#observability.trace(() => Object.freeze({
      name: 'authority_transition',
      transition: 'activation_started',
    }))
    const activation = this.#outputs.activateArtifact(
      operation,
      (action) => this.#receive.startArtifactAuthority(action, attempt.outputFailures),
    )
    activation.then((result) => {
      if (result.kind === 'unavailable') {
        this.#observability.exclude(attempt, 'projection_authority', 'authority_invalidated')
        this.#observability.trace(() => Object.freeze({
          name: 'authority_transition',
          transition: 'authority_invalidated',
        }))
        attempt.close()
        return
      }
      if (result.kind === 'stale') {
        this.#observability.exclude(attempt, 'projection_authority', 'stale_replacement')
        this.#observability.trace(() => Object.freeze({
          name: 'authority_transition',
          transition: 'stale_replacement',
          artifactKind: result.action.artifactKind,
          planKind: result.action.plan.kind,
        }))
        attempt.close()
        return
      }
      return this.#finish(result, attempt)
    }).catch((error: unknown) => {
      if (error instanceof ArtifactChoiceInvalidatedError) {
        this.#observability.exclude(attempt, 'projection_authority', 'authority_invalidated')
      } else if (presentationSourceOutcome(error) === 'picker_refused') {
        this.#observability.exclude(attempt, 'projection_authority', 'picker_refused')
      } else {
        this.#observability.fail(
          attempt,
          'projection_authority',
          error,
          'authority_activation',
        )
        this.#observability.trace(() => Object.freeze({
          name: 'authority_transition',
          transition: 'activation_failed',
        }))
        this.#publishActionError(error)
      }
      attempt.close()
    })
  }

  async #finish(
    activation: AcquiredArtifactActivation,
    attempt: V2PresentationAttempt,
  ): Promise<void> {
    const activeProjection = this.#currentProjection()
    if (
      activeProjection === undefined ||
      activeProjection.epoch !== activation.projectionEpoch ||
      !this.#outputs.acquiredAuthorityIsCurrent(activation)
    ) {
      this.#observability.exclude(attempt, 'projection_authority', 'stale_replacement')
      await Promise.resolve(
        activation.authority.release(new StaleReceiveBoundaryError()),
      ).catch(() => {
        this.#observability.recordConsequence(attempt, 'cleanup')
      })
      attempt.close()
      return
    }

    this.#observability.trace(() => Object.freeze({
      name: 'authority_transition',
      transition: 'authority_acquired',
      artifactKind: activation.action.artifactKind,
      planKind: activation.action.plan.kind,
    }))
    const controller = new AbortController()
    this.abort(new StaleReceiveBoundaryError())
    this.#acquisition = controller
    let frozenIntent: ReceiveIntent | undefined
    let finalizedRuntime: V2BoundReceiveOperation | undefined
    try {
      const runtime = await activation.authority.finalize(async (acquired) => {
        controller.signal.throwIfAborted()
        if (!this.#boundaryIsCurrent(activeProjection, activation)) {
          throw new StaleReceiveBoundaryError()
        }
        const environment = await Promise.resolve(this.#receive.environment(controller.signal))
        controller.signal.throwIfAborted()
        if (!this.#boundaryIsCurrent(activeProjection, activation)) {
          throw new StaleReceiveBoundaryError()
        }
        const bound = await bindMaterialization({
          selection: activeProjection.selection,
          chosenAction: activation.action,
          currentProjection: activeProjection.state.projection,
          currentDiscovery: activeProjection.state.discovery,
          currentEnvironment: environment,
          acquired,
        })
        if (bound.kind !== 'bound') throw new ArtifactChoiceInvalidatedError()
        frozenIntent = bound.intent
        return bound.intent
      }, controller.signal)
      finalizedRuntime = runtime
      controller.signal.throwIfAborted()
      if (!this.#boundaryIsCurrent(activeProjection, activation) || frozenIntent === undefined) {
        this.#observability.exclude(attempt, 'projection_authority', 'stale_replacement')
        await this.#settleAndDetachStaleRuntime(runtime, attempt)
        return
      }

      const intent = await validateReceiveIntent(runtime.intent)
      if (
        intent.digest !== frozenIntent.digest ||
        runtime.lifecycle.operationId !== intent.operationId ||
        runtime.lifecycle.receiveIntentDigest !== intent.digest
      ) {
        throw new TypeError('bound receive runtime does not match the frozen intent authority')
      }
      if (!this.#outputs.adoptReceiveIntent(
        activeProjection.epoch,
        intent,
        runtime.lifecycle,
        Date.now(),
        runtime.initialWorkspaceUsage,
        runtime.activeControls,
      )) {
        this.#observability.exclude(attempt, 'projection_authority', 'stale_replacement')
        await this.#settleAndDetachStaleRuntime(runtime, attempt)
        return
      }

      this.#observability.exclude(attempt, 'projection_authority', 'success')
      this.#observability.trace(() => Object.freeze({
        name: 'authority_transition',
        transition: 'intent_frozen',
        artifactKind: intent.artifact.kind,
        planKind: intent.plan.kind,
      }))
      activeProjection.controller.abort(new DOMException('Receive intent is frozen', 'AbortError'))
      this.#activeReceive.adopt({
        joined: activeProjection.joined,
        selection: activeProjection.frozenSelection,
        runtime,
      })
    } catch (error) {
      await this.#handleActivationFailure(
        error,
        activation,
        attempt,
        activeProjection,
        controller,
        finalizedRuntime,
      )
    } finally {
      if (this.#acquisition === controller) this.#acquisition = undefined
      if (!attempt.decisionSettled) {
        this.#observability.exclude(attempt, 'projection_authority', 'stale_replacement')
      }
      attempt.close()
    }
  }

  async #handleActivationFailure(
    error: unknown,
    activation: AcquiredArtifactActivation,
    attempt: V2PresentationAttempt,
    activeProjection: V2ActiveProjection,
    controller: AbortController,
    finalizedRuntime: V2BoundReceiveOperation | undefined,
  ): Promise<void> {
    const invalidated = error instanceof ArtifactChoiceInvalidatedError
    const sourceOutcome = presentationSourceOutcome(error)
    if (invalidated) {
      this.#observability.exclude(attempt, 'projection_authority', 'authority_invalidated')
    } else if (sourceOutcome === 'picker_refused') {
      this.#observability.exclude(attempt, 'projection_authority', 'picker_refused')
    } else if (
      sourceOutcome === 'caller_cancelled' ||
      sourceOutcome === 'stale_replacement' ||
      controller.signal.aborted ||
      error instanceof StaleReceiveBoundaryError
    ) {
      this.#observability.exclude(attempt, 'projection_authority', 'stale_replacement')
    } else {
      this.#observability.fail(
        attempt,
        'projection_authority',
        error,
        'authority_activation',
      )
      this.#observability.trace(() => Object.freeze({
        name: 'authority_transition',
        transition: 'activation_failed',
        artifactKind: activation.action.artifactKind,
        planKind: activation.action.plan.kind,
      }))
      this.#publishActionError(error)
    }

    if (finalizedRuntime === undefined) {
      await Promise.resolve(activation.authority.release(error)).catch(() => {
        this.#observability.recordConsequence(attempt, 'cleanup')
      })
    } else {
      await Promise.resolve(finalizedRuntime.settleTransferAdmissionFailure(error))
        .catch(() => {
          this.#observability.recordConsequence(attempt, 'settlement')
        })
      await Promise.resolve(finalizedRuntime.detach()).catch(() => {
        this.#observability.recordConsequence(attempt, 'detach')
      })
    }
    if (this.#currentProjection() === activeProjection && !controller.signal.aborted) {
      await this.#refreshCurrentOffers(activeProjection).catch(() => {
        this.#observability.recordConsequence(attempt, 'projection')
      })
    }
  }

  async #refreshCurrentOffers(active: V2ActiveProjection): Promise<void> {
    if (this.#currentProjection() !== active) return
    if (active.protocolSessionId !== active.joined.protocolSessionId) {
      this.#restartProjection(active.joined)
      return
    }
    const environment = await Promise.resolve(this.#receive.environment(active.controller.signal))
    if (this.#currentProjection() !== active || active.controller.signal.aborted) return
    if (active.protocolSessionId !== active.joined.protocolSessionId) {
      this.#restartProjection(active.joined)
      return
    }
    active.environment = environment
    await this.#outputs.updateProjection(active.state, environment)
  }

  #boundaryIsCurrent(
    projection: V2ActiveProjection,
    activation: AcquiredArtifactActivation,
  ): boolean {
    return this.#currentProjection() === projection &&
      projection.epoch === activation.projectionEpoch &&
      this.#currentJoinedShare() === projection.joined &&
      projection.protocolSessionId === projection.joined.protocolSessionId &&
      this.#outputs.acquiredAuthorityIsCurrent(activation)
  }

  async #settleAndDetachStaleRuntime(
    runtime: V2BoundReceiveOperation,
    attempt: V2PresentationAttempt,
  ): Promise<void> {
    await Promise.resolve(
      runtime.settleTransferAdmissionFailure(new StaleReceiveBoundaryError()),
    ).catch(() => {
      this.#observability.recordConsequence(attempt, 'settlement')
    })
    await Promise.resolve(runtime.detach()).catch(() => {
      this.#observability.recordConsequence(attempt, 'detach')
    })
  }
}
