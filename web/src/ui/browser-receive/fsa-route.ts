import { IndexedDbReceiveOperationRepository } from '../../output/browser/indexeddb-repository'
import { IndexedDbCompatibleNameLedger } from '../../output/browser/indexeddb-compatible-name-ledger'
import {
  acquireFSARootMutationLease,
  type FSARootMutationLease,
} from '../../output/browser/namespace-mutation'
import {
  prepareFSAOperationBindingTransition,
  verifyFSAOperationBinding,
  type FSACompatibleNameBootstrapRepository,
} from '../../output/browser/indexeddb-root-binding'
import {
  acquireBrowserReceiveOperationLease,
  verifyBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
  type BrowserReceiveOperationLeaseOptions,
} from '../../output/browser/session-lease'
import { authorizeFSAParent } from '../../output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../output/capability/contract'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type LocalOutputOperationFailureDiagnosticsPort,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import {
  assembleNewFileSystemAccessOutput,
  createFSAAuthorityReference,
  reserveNewFileSystemAccessOutput,
  type FSAFileCheckpointRepositoryFactory,
} from '../../output/file-system-access/session'
import {
  CompatibleNameCoordinator,
  prepareCompatibleNameRootRepair,
  type CompatibleNameActivationLedger,
  type PreparedCompatibleNameRootRepair,
  type CompatibleNameRootRepairRequest,
  type CompatibleNameRootRepairFactory,
} from '../../output/file-system-access/compatible-name/coordinator'
import {
  activatePreparedFileSystemAccessSettlement,
  prepareFileSystemAccessSettlement,
} from '../../output/file-system-access/settlement'
import { TargetOwnershipUnknownError } from '../../output/persistent-tree/errors'
import type {
  BoundReceiveIntent,
  DirectTreeMaterializationRoute,
  FSADirectoryContainerOffer,
  OfferedArtifactChoice,
  ResolvedArtifactAction,
} from '../../output/planning'
import {
  sameArtifactChoiceSemantics,
  sameTargetSemantics,
} from '../../output/planning'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import { decodeStoredReceiveLifecycleState } from '../../output/workspace/state-codec'
import {
  initialReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../../output/workspace/state'
import {
  createDestinationReservationID,
  createOperationID,
  createOutputSessionID,
  createTransferJobID,
  type ArtifactChoiceID,
} from '../../transfer/intent'
import type {
  V2ArtifactPresentationAuthority,
  V2LifecycleMutation,
  V2OwnedActivationAuthority,
  V2RouteCommitInput,
  V2RouteCommitResult,
} from '../v2-receive-runtime'
import { V2PresentationSourceError } from '../v2-receive-runtime'
import { FSAReceiveOperation } from './fsa'
import { FSAResourceOwner } from './fsa-resource-owner'
import { snapshotPreClickRanking } from './shared'

type FSARouteRepository = ReceiveOperationRepository & Partial<FSACompatibleNameBootstrapRepository>
type OfferedFSAChoice = Omit<OfferedArtifactChoice, 'route'> & Readonly<{
  route: Omit<DirectTreeMaterializationRoute, 'target'> & Readonly<{
    target: FSADirectoryContainerOffer
  }>
}>

export interface FSARouteDependencies {
  readonly openRepository: () => Promise<FSARouteRepository>
  readonly authorizeParent: typeof authorizeFSAParent
  readonly acquireRootLease: (
    parent: FileSystemDirectoryHandle,
  ) => Promise<FSARootMutationLease>
  readonly acquireOperationLease: (
    repository: ReceiveOperationRepository,
    operationId: string,
    options: BrowserReceiveOperationLeaseOptions,
  ) => Promise<BrowserReceiveOperationLease>
  readonly prepareCompatibleNameRootRepair: CompatibleNameRootRepairFactory
  readonly openCompatibleNameLedger: () => Promise<CompatibleNameActivationLedger>
  readonly createOperationId: () => string
  readonly createReservationId: () => string
  readonly createAuthorityRef: () => string
  readonly createOutputSessionId: () => string
  readonly createTransferJobId: () => string
  readonly clock: () => number
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
}

export interface FSAArtifactPresentationAuthorityOptions {
  readonly offered: OfferedArtifactChoice
  readonly picked: Promise<AcquiredFSAParentAuthority>
  readonly preClickRanking: readonly ArtifactChoiceID[]
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort
  readonly dependencies?: Partial<FSARouteDependencies>
}

type FSAActivationState = 'open' | 'committing' | 'transferred' | 'released'

export class FSAArtifactPresentationAuthority implements V2ArtifactPresentationAuthority {
  readonly ready: Promise<void>
  readonly #offered: OfferedFSAChoice
  readonly #installedRouteId: string
  readonly #picked: Promise<AcquiredFSAParentAuthority>
  readonly #preClickRanking: readonly ArtifactChoiceID[]
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #localOutputFailures: LocalOutputOperationFailureDiagnosticsPort | undefined
  readonly #dependencies: FSARouteDependencies
  #state: FSAActivationState = 'open'
  #releaseReason: unknown

  constructor(options: FSAArtifactPresentationAuthorityOptions) {
    this.#offered = requireFSAOffer(options.offered)
    this.#preClickRanking = snapshotPreClickRanking(
      this.#offered.choice.choiceId,
      options.preClickRanking,
    )
    this.#installedRouteId = this.#offered.route.target.routeId
    this.#diagnostics = options.diagnostics
    this.#localOutputFailures = options.localOutputFailures
    this.#dependencies = routeDependencies(options.dependencies)
    this.#picked = options.picked.then(
      authority => requirePickedAuthority(authority, this.#offered.route.target),
      (error: unknown) => {
        observeReservationFailure(this.#diagnostics, error)
        throw pickerError(error)
      },
    )
    this.ready = this.#picked.then(() => undefined)
    // A released picker cannot be cancelled, so the route remains its rejection owner.
    this.#picked.catch(() => undefined)
    this.ready.catch(() => undefined)
  }

  async commit(input: V2RouteCommitInput): Promise<V2RouteCommitResult> {
    this.#claimCommit()
    let repository: FSARouteRepository | undefined
    let rootLease: FSARootMutationLease | undefined
    let receiverOperationId: string | undefined
    try {
      const authority = await this.#picked
      this.#requireCommitting(input.signal)
      const artifact = requireResolvedFSAAction(input.action, this.#offered, authority)
      await this.#dependencies.authorizeParent(authority)
      this.#requireCommitting(input.signal)
      repository = await this.#dependencies.openRepository()
      this.#requireCommitting(input.signal)
      rootLease = await this.#dependencies.acquireRootLease(authority.parent)
      this.#requireCommitting(input.signal)
      receiverOperationId = this.#dependencies.createOperationId()
      const reserved = await reserveNewFileSystemAccessOutput({
        authority,
        artifact,
        rootLease,
        operationId: receiverOperationId,
        reservationId: this.#dependencies.createReservationId(),
        authorityRef: this.#dependencies.createAuthorityRef(),
        prepareCompatibleNameRootRepair: this.#dependencies.prepareCompatibleNameRootRepair,
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
      })
      const bound = await input.freezeAtFence(Object.freeze({
        kind: 'destination-reservation',
        targetRouteId: this.#installedRouteId,
        reservation: reserved.reservation,
      }))
      this.#requireCommitting(input.signal)
      requireBoundCandidate(bound, input.action, reserved.reservation.digest)

      const lifecycle = initialReceiveLifecycleState({
        operationId: bound.intent.operationId,
        receiveIntentDigest: bound.intent.digest,
      })
      const preparedBinding = await prepareFSAOperationBindingTransition({
        repository,
        intent: bound.intent,
        parent: authority.parent,
        preClickRanking: this.#preClickRanking,
      })
      const transferJobId = this.#dependencies.createTransferJobId()
      const outputSessionId = this.#dependencies.createOutputSessionId()
      const preparedSettlement = await prepareFileSystemAccessSettlement(bound.intent)
      this.#requireCommitting(input.signal)

      const lease = await this.#dependencies.acquireOperationLease(
        repository,
        bound.intent.operationId,
        {
          acquireTransition: {
            ...preparedBinding.transition,
            lifecycle,
          },
          ...(reserved.compatibleNameRepair === undefined
            ? {}
            : {
                acquisitionTransitionCommitter: compatibleNameBootstrapCommitter(
                  repository,
                  reserved.compatibleNameRepair.bootstrap,
                ),
              }),
        },
      )

      // Returning from lease acquisition is the durable cut, so ownership must exist
      // before any settlement or output-session construction can fail.
      const durableRepository = repository
      const resources = new FSAResourceOwner({
        repository: durableRepository,
        operationLease: lease,
        rootLease,
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
      })
      const owned = new FSAOwnedActivationAuthority(
        bound.intent,
        lifecycle,
        resources,
        () => activatePreparedFileSystemAccessSettlement(preparedSettlement, {
          repository: durableRepository,
          lifecycleLeaseId: lease.leaseId,
          transferJobId,
          clock: this.#dependencies.clock,
          ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
        }),
      )

      try {
        const operation = await this.#completeDurableActivation({
          bound,
          lifecycle,
          repository: durableRepository,
          parent: authority.parent,
          lease,
          rootLease,
          resources,
          owned,
          ...(reserved.compatibleNameRepair === undefined
            ? {}
            : { compatibleNameRepair: reserved.compatibleNameRepair }),
          transferJobId,
          outputSessionId,
          signal: input.signal,
        })
        this.#state = 'transferred'
        return Object.freeze({ kind: 'bound-operation', operation })
      } catch (cause) {
        this.#state = 'transferred'
        return Object.freeze({ kind: 'owned-effects', cause, authority: owned })
      }
    } catch (error) {
      const cleanupFailures = await releasePreCutResources(rootLease, repository, this.#diagnostics)
      if (cleanupFailures.length !== 0) {
        this.#state = 'released'
        throw new AggregateError(
          [error, ...cleanupFailures],
          'FSA activation failed before commit and could not release transient authorities',
          { cause: error },
        )
      }
      if (this.#state === 'committing' && input.signal.aborted) {
        this.#state = 'open'
        return Object.freeze({
          kind: 'retryable-precut',
          ...(receiverOperationId === undefined ? {} : { receiverOperationId }),
        })
      }
      this.#state = 'released'
      throw error
    }
  }

  async #completeDurableActivation(input: Readonly<{
    bound: BoundReceiveIntent
    lifecycle: ReceiveLifecycleState
    repository: FSARouteRepository
    parent: FileSystemDirectoryHandle
    lease: BrowserReceiveOperationLease
    rootLease: FSARootMutationLease
    resources: FSAResourceOwner
    owned: FSAOwnedActivationAuthority
    compatibleNameRepair?: PreparedCompatibleNameRootRepair
    transferJobId: string
    outputSessionId: string
    signal: AbortSignal
  }>) {
    let unadoptedCompatibleNameCoordinator: CompatibleNameCoordinator | undefined
    try {
      const settlement = input.owned.settlementAuthority()
      const binding = await verifyCommittedFSAOwnerCut(
        input.repository,
        input.bound.intent,
        input.parent,
        input.lease,
        input.lifecycle,
      )
      this.#requireCommitting(input.signal)
      unadoptedCompatibleNameCoordinator = input.compatibleNameRepair === undefined
        ? undefined
        : await CompatibleNameCoordinator.activate({
             prepared: input.compatibleNameRepair,
             mutations: input.rootLease.authority,
             pairHandles: input.repository,
             openLedger: this.#dependencies.openCompatibleNameLedger,
          })
      this.#requireCommitting(input.signal)
      const session = await assembleNewFileSystemAccessOutput({
        binding,
        operationRepository: input.repository,
        rootLease: input.rootLease,
        openCompatibleNameLedger: this.#dependencies.openCompatibleNameLedger,
        compatibleNamePreparation: { platform: browserRestorationPlatform() },
        ...(unadoptedCompatibleNameCoordinator === undefined
          ? {}
          : { compatibleNameCoordinator: unadoptedCompatibleNameCoordinator }),
        ...(this.#dependencies.checkpointRepositoryFactory === undefined
          ? {}
          : { checkpointRepositoryFactory: this.#dependencies.checkpointRepositoryFactory }),
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
        ...stageDiagnosticsOption(
          this.#localOutputFailures,
          this.#diagnostics?.failures?.attempt,
          input.transferJobId,
          input.outputSessionId,
        ),
      })
      input.resources.adoptOutputSession(session)
      unadoptedCompatibleNameCoordinator = undefined
      const operation = await FSAReceiveOperation.createCommitted({
        intent: input.bound.intent,
        lifecycle: input.lifecycle,
        repository: input.repository,
        lease: input.lease,
        session,
        settlement,
        transferJobId: input.transferJobId,
        outputSessionId: input.outputSessionId,
        attemptIdentities: Object.freeze({
          createOutputSessionId: this.#dependencies.createOutputSessionId,
          createTransferJobId: this.#dependencies.createTransferJobId,
        }),
        resources: input.resources,
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
        ...localOutputFailuresOption(this.#localOutputFailures),
      })
      this.#requireCommitting(input.signal)
      return operation
    } catch (error) {
      // A coordinator not yet transferred into the output session owns only its ledger handle;
      // the durable bootstrap and any verified partial pair remain recoverable operation state.
      unadoptedCompatibleNameCoordinator?.close()
      throw error
    }
  }

  release(reason: unknown): void {
    if (this.#state === 'transferred' || this.#state === 'released') return
    this.#releaseReason = reason
    this.#state = 'released'
  }

  #claimCommit(): void {
    if (this.#state !== 'open') {
      throw new DOMException('FSA presentation authority is no longer available', 'InvalidStateError')
    }
    this.#state = 'committing'
  }

  #requireCommitting(signal: AbortSignal): void {
    signal.throwIfAborted()
    if (this.#state !== 'committing') {
      throw new V2PresentationSourceError(
        'stale_replacement',
        this.#releaseReason ?? new DOMException(
          'FSA presentation authority was released',
          'AbortError',
        ),
      )
    }
  }
}

class FSAOwnedActivationAuthority implements V2OwnedActivationAuthority {
  readonly intent: BoundReceiveIntent['intent']
  readonly lifecycle: ReceiveLifecycleState
  readonly #resources: FSAResourceOwner
  readonly #createSettlement: () => ReturnType<typeof activatePreparedFileSystemAccessSettlement>
  #settlement: ReturnType<typeof activatePreparedFileSystemAccessSettlement> | undefined

  constructor(
    intent: BoundReceiveIntent['intent'],
    lifecycle: ReceiveLifecycleState,
    resources: FSAResourceOwner,
    createSettlement: () => ReturnType<typeof activatePreparedFileSystemAccessSettlement>,
  ) {
    this.intent = intent
    this.lifecycle = lifecycle
    this.#resources = resources
    this.#createSettlement = createSettlement
  }

  settlementAuthority(): ReturnType<typeof activatePreparedFileSystemAccessSettlement> {
    this.#settlement ??= this.#createSettlement()
    return this.#settlement
  }

  async settleActivationFailure(reason: unknown): Promise<V2LifecycleMutation> {
    const lifecycle = await this.settlementAuthority().settleExecutionAdmissionFailure(
      this.intent,
      reason,
      new AbortController().signal,
    )
    return Object.freeze({ lifecycle, workspaceUsage: null })
  }

  detach(): Promise<void> {
    return this.#resources.close()
  }
}

function requireFSAOffer(offered: OfferedArtifactChoice): OfferedFSAChoice {
  if (offered.choice.artifactKind !== 'directory-tree' ||
      offered.choice.plan.kind !== 'direct-tree' ||
      offered.choice.plan.target.kind !== 'fsa-parent-directory' ||
      offered.route.kind !== 'direct-tree' ||
      offered.route.target.kind !== 'fsa-parent-directory') {
    throw new TypeError('FSA presentation authority requires an offered FSA DirectTree choice')
  }
  return offered as OfferedFSAChoice
}

function requirePickedAuthority(
  authority: AcquiredFSAParentAuthority,
  installedTarget: FSADirectoryContainerOffer,
): AcquiredFSAParentAuthority {
  if (authority.kind !== 'fsa-parent-directory-authority' ||
      authority.targetRouteId !== installedTarget.routeId ||
      authority.offer.routeId !== installedTarget.routeId || authority.parent.kind !== 'directory' ||
      !sameTargetSemantics(authority.offer, installedTarget)) {
    throw new TypeError('Picked FSA authority does not match the installed route identity')
  }
  return authority
}

function requireResolvedFSAAction(
  action: ResolvedArtifactAction,
  offered: OfferedFSAChoice,
  authority: AcquiredFSAParentAuthority,
): Extract<ResolvedArtifactAction['artifact'], { kind: 'directory-tree' }> {
  if (action.kind !== 'resolved-artifact-action' ||
      action.choice.artifactKind !== 'directory-tree' || action.artifact.kind !== 'directory-tree' ||
      action.artifact.layout.kind === 'catalog-root' || action.route.kind !== 'direct-tree' ||
      action.route.target.kind !== 'fsa-parent-directory' ||
      action.route.target.routeId !== offered.route.target.routeId ||
      action.resolvedArtifactDigest !== action.artifact.digest ||
      authority.targetRouteId !== action.route.target.routeId ||
      !sameArtifactChoiceSemantics(action.choice, offered.choice) ||
      !sameTargetSemantics(action.route.target, authority.offer)) {
    throw new TypeError('Resolved action does not match the installed FSA DirectTree route')
  }
  return action.artifact
}

function requireBoundCandidate(
  bound: BoundReceiveIntent,
  action: ResolvedArtifactAction,
  reservationDigest: string,
): void {
  if (bound.intent.operationId !== bound.decision.operation_id ||
      bound.intent.digest !== bound.decision.receive_intent_digest ||
      bound.intent.artifact.digest !== action.artifact.digest ||
      bound.intent.plan.kind !== 'direct-tree' ||
      bound.intent.plan.reservation.digest !== reservationDigest) {
    throw new TypeError('Frozen ReceiveIntent does not bind the prepared FSA candidate')
  }
}

async function verifyInitialLifecycle(
  repository: Pick<ReceiveOperationRepository, 'readLifecycle'>,
  expected: ReceiveLifecycleState,
): Promise<void> {
  const record = await repository.readLifecycle(expected.operationId)
  if (record === undefined) {
    throw new DOMException('Initial receive lifecycle is missing after commit', 'InvalidStateError')
  }
  const current = decodeStoredReceiveLifecycleState(record)
  if (current.kind !== 'intent-frozen' || current.operationId !== expected.operationId ||
      current.receiveIntentDigest !== expected.receiveIntentDigest ||
      current.generation !== expected.generation) {
    throw new DOMException('Initial receive lifecycle changed during commit', 'InvalidStateError')
  }
}

async function verifyCommittedFSAOwnerCut(
  repository: FSARouteRepository,
  intent: BoundReceiveIntent['intent'],
  parent: FileSystemDirectoryHandle,
  lease: BrowserReceiveOperationLease,
  lifecycle: ReceiveLifecycleState,
) {
  try {
    const [binding] = await Promise.all([
      verifyFSAOperationBinding({ repository, intent, expectedParent: parent }),
      verifyBrowserReceiveOperationLease(repository, lease),
      verifyInitialLifecycle(repository, lifecycle),
    ])
    return binding
  } catch (cause) {
    if (cause instanceof TargetOwnershipUnknownError) throw cause
    throw new TargetOwnershipUnknownError('reservation', intent.operationId, { cause })
  }
}

async function releasePreCutResources(
  rootLease: FSARootMutationLease | undefined,
  repository: FSARouteRepository | undefined,
  diagnostics: OutputDiagnosticsPorts | undefined,
): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  try {
    await rootLease?.release()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  try {
    repository?.close()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  if (failures.length !== 0) {
    emitOutputTrace(diagnostics?.trace, () => outputTraceEvent('cleanup', {
      backend: 'file_system_access',
      transition: 'failed',
    }))
  }
  return Object.freeze(failures)
}

function observeReservationFailure(
  diagnostics: OutputDiagnosticsPorts | undefined,
  error: unknown,
): void {
  recordOutputException(diagnostics?.failures?.outputReservation, error)
  emitOutputTrace(diagnostics?.trace, () => outputTraceEvent('output_reservation', {
    backend: 'file_system_access',
    transition: 'failed',
  }))
}

function pickerError(error: unknown): unknown {
  return error instanceof DOMException && error.name === 'AbortError'
    ? new V2PresentationSourceError('picker_refused', error)
    : error
}

function stageDiagnosticsOption(
  failures: LocalOutputOperationFailureDiagnosticsPort | undefined,
  attempt: NonNullable<OutputDiagnosticsPorts['failures']>['attempt'],
  transferJobId: string,
  outputSessionId: string,
): Readonly<{
  stageDiagnostics?: ReturnType<LocalOutputOperationFailureDiagnosticsPort['forAttempt']>
}> {
  return failures === undefined || attempt === undefined
    ? Object.freeze({})
    : Object.freeze({
        stageDiagnostics: failures.forAttempt({ attempt, transferJobId, outputSessionId }),
      })
}

function localOutputFailuresOption(
  failures: LocalOutputOperationFailureDiagnosticsPort | undefined,
): Readonly<{ localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort }> {
  return failures === undefined
    ? Object.freeze({})
    : Object.freeze({ localOutputFailures: failures })
}

function routeDependencies(
  overrides: Partial<FSARouteDependencies> | undefined,
): FSARouteDependencies {
  return Object.freeze({
    openRepository: () => IndexedDbReceiveOperationRepository.open(),
    authorizeParent: authorizeFSAParent,
    acquireRootLease: (parent: FileSystemDirectoryHandle) => acquireFSARootMutationLease(parent),
    acquireOperationLease: acquireBrowserReceiveOperationLease,
    prepareCompatibleNameRootRepair: (input: CompatibleNameRootRepairRequest) =>
      prepareCompatibleNameRootRepair(input, {
        platform: browserRestorationPlatform(),
      }),
    openCompatibleNameLedger: () => IndexedDbCompatibleNameLedger.open(),
    createOperationId: createOperationID,
    createReservationId: createDestinationReservationID,
    createAuthorityRef: createFSAAuthorityReference,
    createOutputSessionId: createOutputSessionID,
    createTransferJobId: createTransferJobID,
    clock: Date.now,
    ...overrides,
  })
}

function compatibleNameBootstrapCommitter(
  repository: FSARouteRepository,
  bootstrap: NonNullable<Awaited<ReturnType<CompatibleNameRootRepairFactory>>>['bootstrap'],
) {
  if (repository.commitFSACompatibleNameBootstrap === undefined) {
    throw new DOMException(
      'The FSA repository cannot atomically commit a compatible-name bootstrap',
      'NotSupportedError',
    )
  }
  return Object.freeze({
    commitAcquisitionTransition: (transition: Parameters<
      FSACompatibleNameBootstrapRepository['commitFSACompatibleNameBootstrap']
    >[0]['transition']) => repository.commitFSACompatibleNameBootstrap!({ transition, bootstrap }),
  })
}

function browserRestorationPlatform(): string {
  const platform = globalThis.navigator?.platform
  return typeof platform === 'string' && platform.toLowerCase().startsWith('win')
    ? 'windows'
    : 'unsupported'
}
