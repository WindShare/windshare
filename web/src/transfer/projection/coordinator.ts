import {
  SelectionProjectionError,
  type AuthenticatedGenerationReference,
  type AuthenticatedProjectionEvidence,
  type ProjectionEpoch,
  type RetryableDiscoveryReason,
  type SelectionProjectionState,
  type SettledLayoutBasisProof,
  type UnsettledSelectionTarget,
  type WorkspaceCostObservationV1,
} from './model'
import { SelectionProjectionController } from './reducer'

export interface AuthenticatedDiscoveryRequest {
  readonly epoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly retainedGenerations: readonly AuthenticatedGenerationReference[]
  readonly unsettledTargets: readonly UnsettledSelectionTarget[]
  readonly signal: AbortSignal
}

export interface AuthenticatedDiscoveryCompletion {
  readonly settledTargets?: readonly UnsettledSelectionTarget[]
  readonly layoutBasis?: SettledLayoutBasisProof
  readonly workspaceCostObservation?: WorkspaceCostObservationV1
}

export interface AuthenticatedDiscoverySource {
  /** Every yielded batch must derive only from committed, validated catalog generations. */
  discover(
    request: AuthenticatedDiscoveryRequest,
  ): AsyncGenerator<AuthenticatedProjectionEvidence, AuthenticatedDiscoveryCompletion>
}

export class RetryableProjectionDiscoveryError extends Error {
  readonly reason: RetryableDiscoveryReason

  constructor(reason: RetryableDiscoveryReason, options?: ErrorOptions) {
    super('selection projection discovery can be retried', options)
    this.name = 'RetryableProjectionDiscoveryError'
    this.reason = reason
  }
}

type DiscoveryStep =
  | Readonly<{
      kind: 'stale'
      eventClass: 'catalog-evidence' | 'discovery-result'
    }>
  | Readonly<{ kind: 'evidence'; evidence: AuthenticatedProjectionEvidence }>
  | Readonly<{ kind: 'complete'; completion: AuthenticatedDiscoveryCompletion }>
  | Readonly<{ kind: 'retryable-failure'; error: RetryableProjectionDiscoveryError }>

/**
 * The iterator yields data snapshots only. Keeping activation-bearing callbacks out
 * of this boundary prevents background discovery from acquiring output authority.
 */
export async function* discoverAuthenticatedSelection(
  controller: SelectionProjectionController,
  source: AuthenticatedDiscoverySource,
  signal: AbortSignal,
): AsyncGenerator<SelectionProjectionState, SelectionProjectionState> {
  const epoch = controller.state.projection.epoch
  yield controller.apply(Object.freeze({ kind: 'discovery-started', epoch }))
  return yield* consumeDiscovery(controller, source, signal, epoch)
}

export async function* retryAuthenticatedSelectionDiscovery(
  controller: SelectionProjectionController,
  source: AuthenticatedDiscoverySource,
  signal: AbortSignal,
): AsyncGenerator<SelectionProjectionState, SelectionProjectionState> {
  const epoch = controller.state.projection.epoch
  yield controller.apply(Object.freeze({ kind: 'retry-started', epoch }))
  return yield* consumeDiscovery(controller, source, signal, epoch)
}

async function* consumeDiscovery(
  controller: SelectionProjectionController,
  source: AuthenticatedDiscoverySource,
  signal: AbortSignal,
  epoch: ProjectionEpoch,
): AsyncGenerator<SelectionProjectionState, SelectionProjectionState> {
  const initial = controller.state.projection
  const iterator = source.discover(Object.freeze({
    epoch,
    selectionDigest: initial.selectionDigest,
    retainedGenerations: initial.generations,
    unsettledTargets: initial.unsettledTargets,
    signal,
  }))
  while (true) {
    const step = await nextDiscoveryStep(controller, iterator, signal, epoch)
    switch (step.kind) {
      case 'stale':
        controller.dropStaleAsyncEvent(epoch, step.eventClass)
        await iterator.return(Object.freeze({}))
        return controller.state
      case 'evidence': {
        const before = controller.state
        const after = controller.apply(Object.freeze({
          kind: 'authenticated-evidence',
          epoch,
          evidence: step.evidence,
        }))
        if (after !== before) yield after
        break
      }
      case 'complete': {
        const completed = applyCompletion(controller, epoch, step.completion)
        yield completed
        return completed
      }
      case 'retryable-failure': {
        const failed = applyRetryableFailure(controller, epoch, step.error)
        yield failed
        return failed
      }
    }
  }
}

async function nextDiscoveryStep(
  controller: SelectionProjectionController,
  iterator: AsyncGenerator<AuthenticatedProjectionEvidence, AuthenticatedDiscoveryCompletion>,
  signal: AbortSignal,
  epoch: ProjectionEpoch,
): Promise<DiscoveryStep> {
  signal.throwIfAborted()
  let next: IteratorResult<AuthenticatedProjectionEvidence, AuthenticatedDiscoveryCompletion>
  try {
    next = await iterator.next()
  } catch (error) {
    if (controller.state.projection.epoch !== epoch) {
      return Object.freeze({ kind: 'stale', eventClass: 'discovery-result' })
    }
    if (error instanceof RetryableProjectionDiscoveryError) {
      return Object.freeze({ kind: 'retryable-failure', error })
    }
    throw error
  }
  signal.throwIfAborted()
  if (controller.state.projection.epoch !== epoch) {
    return Object.freeze({
      kind: 'stale',
      eventClass: next.done ? 'discovery-result' : 'catalog-evidence',
    })
  }
  return next.done
    ? Object.freeze({ kind: 'complete', completion: next.value })
    : Object.freeze({ kind: 'evidence', evidence: next.value })
}

function applyCompletion(
  controller: SelectionProjectionController,
  epoch: ProjectionEpoch,
  completion: AuthenticatedDiscoveryCompletion,
): SelectionProjectionState {
  return controller.apply(Object.freeze({
    kind: 'discovery-completed',
    epoch,
    ...(completion.settledTargets === undefined
      ? {}
      : { settledTargets: completion.settledTargets }),
    ...(completion.layoutBasis === undefined ? {} : { layoutBasis: completion.layoutBasis }),
    ...(completion.workspaceCostObservation === undefined
      ? {}
      : { workspaceCostObservation: completion.workspaceCostObservation }),
  }))
}

function applyRetryableFailure(
  controller: SelectionProjectionController,
  epoch: ProjectionEpoch,
  error: RetryableProjectionDiscoveryError,
): SelectionProjectionState {
  if (controller.state.discovery.kind !== 'discovering') {
    throw new SelectionProjectionError('retryable source failure escaped active discovery', {
      cause: error,
    })
  }
  return controller.apply(Object.freeze({
    kind: 'retryable-failure',
    epoch,
    reason: error.reason,
  }))
}
