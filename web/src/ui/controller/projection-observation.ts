import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import {
  createSelectionSpec,
  selectionRulesSpecFromPolicy,
  type SelectionSpec,
} from '../../transfer/intent'
import {
  discoverAuthenticatedSelection,
  retryAuthenticatedSelectionDiscovery,
  SelectionProjectionController,
  type ProjectionEpoch,
  type SelectionProjectionState,
} from '../../transfer/projection'
import type { EnvironmentOffers } from '../../output/planning'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type { V2ReceiveCompositionPort } from '../v2-receive-runtime'
import {
  StaleReceiveBoundaryError,
  type V2ReceiverControllerOptions,
} from './contracts'
import type { V2ControllerObservability } from './controller-observability'

export interface V2ActiveProjection {
  /** Selection/projection replacements have distinct owner revisions. */
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

export interface ProjectionObservationAuthority {
  observeProjection(active: V2ActiveProjection): void
  startObservationReplacement(reason: unknown): void
  invalidate(reason: unknown, invalidation: 'selection-changed'): void
}

export interface SelectionProjectionRuntimeOptions {
  readonly receive: Pick<V2ReceiveCompositionPort, 'environment'>
  readonly authority: ProjectionObservationAuthority
  readonly observability: V2ControllerObservability
  readonly currentJoinedShare: () => V2JoinedBrowserShare | undefined
  readonly isDisposed: () => boolean
  readonly onFailure: (error: unknown) => void
  readonly trace?: V2ReceiverControllerOptions['trace']
}

export type ProjectionReplacementKind = 'selection-change' | 'observation-replacement'

/** Owns replaceable network observations without acquiring or publishing local output authority. */
export class SelectionProjectionRuntime {
  readonly #options: SelectionProjectionRuntimeOptions
  readonly #projection: SelectionProjectionController
  #revision = 0
  #pending: AbortController | undefined
  #active: V2ActiveProjection | undefined

  constructor(options: SelectionProjectionRuntimeOptions) {
    this.#options = options
    this.#projection = new SelectionProjectionController(options.trace)
  }

  get current(): V2ActiveProjection | undefined {
    return this.#active
  }

  start(joined: V2JoinedBrowserShare, replacement: ProjectionReplacementKind): void {
    const revision = ++this.#revision
    const reason = new StaleReceiveBoundaryError()
    if (replacement === 'observation-replacement') {
      this.#options.authority.startObservationReplacement(reason)
    } else {
      this.#options.authority.invalidate(reason, 'selection-changed')
    }
    this.#pending?.abort(reason)
    this.#active?.controller.abort(reason)
    this.#pending = undefined
    this.#active = undefined

    const attempt = this.#options.observability.open('projection')
    const controller = new AbortController()
    this.#pending = controller
    const frozenSelection = joined.selection.snapshot()
    const environment = Promise.resolve(this.#options.receive.environment(controller.signal))
    this.#startOwned(revision, joined, frozenSelection, controller, environment).then(() => {
      this.#options.observability.exclude(
        attempt,
        'projection_authority',
        this.#boundaryIsCurrent(revision, joined) && !controller.signal.aborted
          ? 'success'
          : 'stale_replacement',
      )
    }).catch((error: unknown) => {
      if (!controller.signal.aborted && this.#boundaryIsCurrent(revision, joined)) {
        this.#options.observability.fail(attempt, 'projection_authority', error, 'projection')
        this.#options.onFailure(error)
      } else {
        this.#options.observability.exclude(attempt, 'projection_authority', 'stale_replacement')
      }
    }).finally(() => {
      if (this.#pending === controller) this.#pending = undefined
      attempt.close()
    })
  }

  async retry(active: V2ActiveProjection): Promise<void> {
    const attempt = this.#options.observability.open('projection')
    const reason = new DOMException('Projection retry replaced the prior source', 'AbortError')
    this.#options.authority.startObservationReplacement(reason)
    active.controller.abort(reason)
    const controller = new AbortController()
    active.controller = controller
    try {
      const environment = await Promise.resolve(this.#options.receive.environment(controller.signal))
      controller.signal.throwIfAborted()
      if (!this.#owns(active, controller)) {
        this.#options.observability.exclude(attempt, 'projection_authority', 'stale_replacement')
        return
      }
      active.environment = environment
      active.protocolSessionId = active.joined.protocolSessionId
      await this.#consume(
        active,
        controller,
        retryAuthenticatedSelectionDiscovery(
          this.#projection,
          active.joined.projectionSource(active.frozenSelection, true),
          controller.signal,
        ),
      )
      this.#options.observability.exclude(attempt, 'projection_authority', 'success')
    } catch (error) {
      if (!controller.signal.aborted && this.#active === active && active.controller === controller) {
        this.#options.observability.fail(attempt, 'projection_authority', error, 'projection')
        this.#options.onFailure(error)
      } else {
        this.#options.observability.exclude(attempt, 'projection_authority', 'stale_replacement')
      }
      throw error
    } finally {
      attempt.close()
    }
  }

  stop(reason: unknown): void {
    this.#revision += 1
    this.#pending?.abort(reason)
    this.#pending = undefined
    this.#active?.controller.abort(reason)
    this.#active = undefined
  }

  async #startOwned(
    revision: number,
    joined: V2JoinedBrowserShare,
    frozenSelection: V2FrozenSelectionPolicy,
    controller: AbortController,
    environmentTask: Promise<EnvironmentOffers>,
  ): Promise<void> {
    const selection = await createSelectionSpec({
      shareInstance: joined.descriptor.shareInstanceId,
      syntheticRoot: joined.descriptor.syntheticRootId,
      rules: selectionRulesSpecFromPolicy(frozenSelection),
    })
    controller.signal.throwIfAborted()
    if (!this.#boundaryIsCurrent(revision, joined)) return
    const environment = await environmentTask
    controller.signal.throwIfAborted()
    if (!this.#boundaryIsCurrent(revision, joined)) return

    const protocolSessionId = joined.protocolSessionId
    const state = this.#projection.beginSelection(selection, Object.freeze({
      protocolSessionId: joined.protocolSessionIdentity,
    }))
    const active: V2ActiveProjection = {
      revision,
      joined,
      selection,
      frozenSelection,
      epoch: state.projection.epoch,
      controller,
      protocolSessionId,
      state,
      environment,
    }
    this.#active = active
    if (this.#pending === controller) this.#pending = undefined
    this.#options.authority.observeProjection(active)
    if (!this.#owns(active, controller)) return
    if (active.protocolSessionId !== joined.protocolSessionId) {
      this.start(joined, 'observation-replacement')
      return
    }
    await this.#consume(
      active,
      controller,
      discoverAuthenticatedSelection(
        this.#projection,
        joined.projectionSource(frozenSelection),
        controller.signal,
      ),
    )
  }

  async #consume(
    active: V2ActiveProjection,
    controller: AbortController,
    states: AsyncGenerator<SelectionProjectionState, SelectionProjectionState>,
  ): Promise<void> {
    for await (const state of states) {
      if (!this.#owns(active, controller)) return
      active.state = state
      this.#options.authority.observeProjection(active)
    }
  }

  #owns(active: V2ActiveProjection, controller: AbortController): boolean {
    return this.#active === active && active.controller === controller &&
      !controller.signal.aborted && this.#boundaryIsCurrent(active.revision, active.joined)
  }

  #boundaryIsCurrent(revision: number, joined: V2JoinedBrowserShare): boolean {
    return revision === this.#revision && this.#options.currentJoinedShare() === joined &&
      !this.#options.isDisposed()
  }
}
