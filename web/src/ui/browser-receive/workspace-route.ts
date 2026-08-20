import { encodeBase64Url } from '../../crypto/bytes'
import { IndexedDbReceiveOperationRepository } from '../../output/browser/indexeddb-repository'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import {
  openOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from '../../output/origin-private/namespace'
import {
  sameArtifactChoiceSemantics,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../output/planning'
import {
  WorkspaceOperationStages,
  type WorkspaceContentRequestCounter,
  type WorkspaceStageTraceListener,
} from '../../output/workspace/stages'
import type {
  ReceiveOperationRepository,
  WorkspaceActivationJournalRepository,
} from '../../output/workspace/repository'
import {
  createOperationID,
  createWorkspaceBinding,
  createWorkspaceID,
  type ReceiveIntent,
} from '../../transfer/intent'
import type {
  V2ArtifactPresentationAuthority,
  V2BoundReceiveOperation,
  V2RouteCommitInput,
  V2RouteCommitResult,
} from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import {
  workspaceActivationOwnerDependencies,
  WorkspaceOwnedActivationAuthority,
  type WorkspaceActivationOwnerContext,
  type WorkspaceActivationOwnerDependencies,
} from './workspace-activation-owner'
import { WorkspaceReceiveOperation } from './workspace-operation'

const ZERO_CONTENT_REQUESTS: WorkspaceContentRequestCounter = Object.freeze({ count: () => 0n })
const WORKSPACE_REPOSITORY_REFERENCE_BYTES = 32

type OpenWorkspaceNamespaceInput = Parameters<typeof openOriginPrivateWorkspaceNamespace>[0]
type CreateWorkspaceOperationInput = Parameters<typeof WorkspaceReceiveOperation.create>[0]
type WorkspaceCommitAttempt = {
  repository?: WorkspaceActivationJournalRepository
  frozen?: Awaited<ReturnType<V2RouteCommitInput['freezeAtFence']>>
  namespace?: OriginPrivateWorkspaceNamespace
  owner?: WorkspaceOwnedActivationAuthority
}

export interface WorkspaceRouteDependencies {
  readonly openRepository: () => Promise<WorkspaceActivationJournalRepository>
  readonly openNamespace: (
    input: OpenWorkspaceNamespaceInput,
  ) => Promise<OriginPrivateWorkspaceNamespace>
  readonly openStages: typeof WorkspaceOperationStages.open
  readonly createOperation: (
    input: CreateWorkspaceOperationInput,
  ) => Promise<V2BoundReceiveOperation>
  readonly operationId: typeof createOperationID
  readonly workspaceId: typeof createWorkspaceID
  readonly repositoryReference: () => string
  readonly activationOwner: WorkspaceActivationOwnerDependencies
}

export type WorkspaceRouteDependencyOverrides =
  Partial<Omit<WorkspaceRouteDependencies, 'activationOwner'>> &
  Readonly<{ activationOwner?: Partial<WorkspaceActivationOwnerDependencies> }>

export interface WorkspaceArtifactAuthorityOptions {
  readonly windowPort: BrowserReceiveWindow
  readonly offered: OfferedArtifactChoice
  readonly trace?: WorkspaceStageTraceListener
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly dependencies?: WorkspaceRouteDependencyOverrides
}

/** Workspace has no click-time external authority, so readiness is immediate. */
export class WorkspaceArtifactPresentationAuthority implements V2ArtifactPresentationAuthority {
  readonly ready = Promise.resolve()
  readonly #window: BrowserReceiveWindow
  readonly #choice: OfferedArtifactChoice['choice']
  readonly #workspaceRouteId: string
  readonly #publicationTargetRouteId: string
  readonly #trace: WorkspaceStageTraceListener | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #dependencies: WorkspaceRouteDependencies
  #released = false
  #claimed = false
  #preparedIdentity: Readonly<{
    operationId: string
    workspaceId: string
    repositoryRef: string
  }> | undefined

  constructor(options: WorkspaceArtifactAuthorityOptions) {
    const route = options.offered.route
    if (options.offered.choice.plan.kind !== 'workspace-then-publish' ||
        route.kind !== 'workspace-then-publish' ||
        options.offered.choice.artifactKind === 'directory-tree') {
      throw unavailableWorkspaceRoute()
    }
    this.#window = options.windowPort
    this.#choice = options.offered.choice
    this.#workspaceRouteId = route.workspace.routeId
    this.#publicationTargetRouteId = route.publicationTarget.routeId
    this.#trace = options.trace
    this.#diagnostics = options.diagnostics
    this.#dependencies = workspaceRouteDependencies(options.windowPort, options.dependencies)
  }

  async commit(input: V2RouteCommitInput): Promise<V2RouteCommitResult> {
    this.#claim()
    const action = this.#requireAction(input.action)
    const identity = this.#preparedIdentity ??= Object.freeze({
      operationId: this.#dependencies.operationId(),
      workspaceId: this.#dependencies.workspaceId(),
      repositoryRef: this.#dependencies.repositoryReference(),
    })
    const operationId = identity.operationId
    const workspace = await createWorkspaceBinding({
      operationId,
      workspaceId: identity.workspaceId,
      artifact: action.artifact,
      repositoryRef: identity.repositoryRef,
    })
    const attempt: WorkspaceCommitAttempt = {}
    try {
      return await this.#performCommit(input, workspace, operationId, attempt)
    } catch (error) {
      return this.#handleCommitFailure(input, operationId, attempt, error)
    }
  }

  async #performCommit(
    input: V2RouteCommitInput,
    workspace: Awaited<ReturnType<typeof createWorkspaceBinding>>,
    operationId: string,
    attempt: WorkspaceCommitAttempt,
  ): Promise<V2RouteCommitResult> {
    input.signal.throwIfAborted()
    attempt.frozen = await input.freezeAtFence(Object.freeze({
      kind: 'workspace-binding',
      workspaceRouteId: this.#workspaceRouteId,
      publicationTargetRouteId: this.#publicationTargetRouteId,
      workspace,
    }))
    input.signal.throwIfAborted()
    this.#requireLive()
    attempt.repository = await this.#dependencies.openRepository()
    input.signal.throwIfAborted()
    this.#requireLive()
    const lease = await this.#dependencies.activationOwner.withActivationLock(operationId, async () => {
      attempt.namespace = await this.#dependencies.openNamespace({
        receiveIntent: attempt.frozen!.intent,
        repository: attempt.repository!,
        storage: this.#window.navigator.storage,
        signal: input.signal,
        onActivationCandidateCommitted: candidate => {
          attempt.owner = WorkspaceOwnedActivationAuthority.fromCandidate(
            this.#ownerContext(attempt.frozen!.intent, attempt.repository!),
            candidate,
          )
        },
        onPersistenceCommitted: persisted => this.#adoptPersistedNamespace(attempt, persisted),
      })
      attempt.owner = await this.#requireDurableOwner(
        attempt,
        attempt.repository!,
        attempt.frozen!,
        attempt.namespace,
      )
      input.signal.throwIfAborted()
      this.#requireLive()
      return attempt.owner.acquireLease()
    })
    const namespace = attempt.namespace
    const owner = attempt.owner
    if (namespace === undefined || owner === undefined) {
      throw new DOMException('Workspace activation authority was not established', 'InvalidStateError')
    }
    const stages = await this.#dependencies.openStages({
      repository: attempt.repository,
      receiveIntent: attempt.frozen.intent,
      leaseId: lease.leaseId,
      clock: Date.now,
      contentRequests: ZERO_CONTENT_REQUESTS,
      ...(this.#trace === undefined ? {} : { onTrace: this.#trace }),
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
    const runtime = await this.#dependencies.createOperation({
      windowPort: this.#window,
      intent: attempt.frozen.intent,
      repository: attempt.repository,
      namespace,
      lease,
      stages,
      ...(this.#trace === undefined ? {} : { trace: this.#trace }),
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
    input.signal.throwIfAborted()
    this.#requireLive()
    owner.transferToBoundOperation()
    emitOutputTrace(this.#diagnostics?.trace, () =>
      outputTraceEvent('output_reservation', { backend: 'origin_private', transition: 'acquired' }))
    return Object.freeze({ kind: 'bound-operation', operation: runtime })
  }

  #adoptPersistedNamespace(
    attempt: WorkspaceCommitAttempt,
    namespace: OriginPrivateWorkspaceNamespace,
  ): void {
    if (attempt.owner === undefined) {
      attempt.owner = WorkspaceOwnedActivationAuthority.fromPersistedNamespace(
        this.#ownerContext(attempt.frozen!.intent, attempt.repository!),
        namespace,
      )
      return
    }
    attempt.owner.adoptNamespace(namespace, true)
  }

  async #requireDurableOwner(
    attempt: WorkspaceCommitAttempt,
    repository: WorkspaceActivationJournalRepository,
    frozen: Awaited<ReturnType<V2RouteCommitInput['freezeAtFence']>>,
    namespace: OriginPrivateWorkspaceNamespace,
  ): Promise<WorkspaceOwnedActivationAuthority> {
    if (attempt.owner !== undefined) {
      attempt.owner.adoptNamespace(namespace, true)
      return attempt.owner
    }
    return WorkspaceOwnedActivationAuthority.requirePersistedNamespace(
      this.#ownerContext(frozen.intent, repository),
      namespace,
    )
  }

  async #handleCommitFailure(
    input: V2RouteCommitInput,
    operationId: string,
    attempt: WorkspaceCommitAttempt,
    error: unknown,
  ): Promise<V2RouteCommitResult> {
    observeWorkspaceReservationFailure(this.#diagnostics, error)
    if (attempt.repository === undefined || attempt.frozen === undefined) {
      if (this.#canRetryPrecut(input.signal)) return this.#retryablePrecut(operationId)
      throw error
    }
    const authority = await WorkspaceOwnedActivationAuthority.recover({
      intent: attempt.frozen.intent,
      repository: attempt.repository,
      storage: this.#window.navigator.storage,
      dependencies: this.#dependencies.activationOwner,
      namespace: attempt.namespace,
      owner: attempt.owner,
      error,
      ...(this.#trace === undefined ? {} : { trace: this.#trace }),
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
    if (authority !== undefined) {
      return Object.freeze({ kind: 'owned-effects', cause: error, authority })
    }
    closeRepositoryAfterCleanFailure(attempt.repository, this.#diagnostics, error)
    if (this.#canRetryPrecut(input.signal)) return this.#retryablePrecut(operationId)
    throw error
  }

  #retryablePrecut(operationId: string): V2RouteCommitResult {
    this.#claimed = false
    return Object.freeze({ kind: 'retryable-precut', receiverOperationId: operationId })
  }

  release(): void {
    this.#released = true
  }

  #claim(): void {
    if (this.#claimed) {
      throw new DOMException('Workspace artifact authority was already committed', 'InvalidStateError')
    }
    this.#claimed = true
    this.#requireLive()
  }

  #requireLive(): void {
    if (this.#released) throw new DOMException('Artifact authority was released', 'AbortError')
  }

  #requireAction(action: ResolvedArtifactAction): ResolvedArtifactAction & Readonly<{
    route: Extract<ResolvedArtifactAction['route'], { kind: 'workspace-then-publish' }>
  }> {
    if (action.route.kind !== 'workspace-then-publish' ||
        action.artifact.kind === 'directory-tree' ||
        action.route.workspace.routeId !== this.#workspaceRouteId ||
        action.route.publicationTarget.routeId !== this.#publicationTargetRouteId ||
        !sameArtifactChoiceSemantics(action.choice, this.#choice)) {
      throw unavailableWorkspaceRoute()
    }
    return action as ResolvedArtifactAction & Readonly<{
      route: Extract<ResolvedArtifactAction['route'], { kind: 'workspace-then-publish' }>
    }>
  }

  #ownerContext(
    intent: ReceiveIntent,
    repository: WorkspaceActivationJournalRepository,
  ): WorkspaceActivationOwnerContext {
    return Object.freeze({
      intent,
      repository,
      storage: this.#window.navigator.storage,
      dependencies: this.#dependencies.activationOwner,
      ...(this.#trace === undefined ? {} : { trace: this.#trace }),
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
  }

  #canRetryPrecut(signal: AbortSignal): boolean {
    return signal.aborted && !this.#released
  }
}

export function startWorkspaceArtifactAuthority(
  options: WorkspaceArtifactAuthorityOptions,
): V2ArtifactPresentationAuthority {
  return new WorkspaceArtifactPresentationAuthority(options)
}

function workspaceRouteDependencies(
  windowPort: BrowserReceiveWindow,
  overrides: WorkspaceRouteDependencyOverrides | undefined,
): WorkspaceRouteDependencies {
  return Object.freeze({
    openRepository: () => IndexedDbReceiveOperationRepository.open(),
    openNamespace: openOriginPrivateWorkspaceNamespace,
    openStages: WorkspaceOperationStages.open,
    createOperation: (input: CreateWorkspaceOperationInput) =>
      WorkspaceReceiveOperation.create(input),
    operationId: createOperationID,
    workspaceId: createWorkspaceID,
    repositoryReference: randomWorkspaceRepositoryReference,
    ...overrides,
    activationOwner: workspaceActivationOwnerDependencies(windowPort, overrides?.activationOwner),
  })
}

function randomWorkspaceRepositoryReference(): string {
  const bytes = new Uint8Array(WORKSPACE_REPOSITORY_REFERENCE_BYTES)
  crypto.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}

function closeRepositoryAfterCleanFailure(
  repository: ReceiveOperationRepository,
  diagnostics: OutputDiagnosticsPorts | undefined,
  cause: unknown,
): void {
  try {
    repository.close()
  } catch (closeFailure) {
    recordOutputException(diagnostics?.failures?.cleanup, closeFailure)
    throw new AggregateError(
      [cause, closeFailure],
      'Workspace activation failed before persistence and its repository did not close',
      { cause: closeFailure },
    )
  }
}

function observeWorkspaceReservationFailure(
  diagnostics: OutputDiagnosticsPorts | undefined,
  error: unknown,
): void {
  recordOutputException(diagnostics?.failures?.outputReservation, error)
  emitOutputTrace(diagnostics?.trace, () =>
    outputTraceEvent('output_reservation', {
      backend: 'origin_private',
      transition: 'failed',
    }))
}

function unavailableWorkspaceRoute(): DOMException {
  return new DOMException('No installed workspace authority matches this action', 'NotSupportedError')
}
