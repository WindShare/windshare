import { acquireFSARootMutationLease } from '../../src/output/browser/namespace-mutation'
import { PathComponentRejectedError } from '../../src/output/browser/filesystem-component-inspection'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import {
  FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT,
  FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR,
} from '../../src/output/browser/indexeddb-root-binding'
import { createIncidentScopeIssuer } from '../../src/diagnostics/incident'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../src/output/planning'
import {
  createAttemptOutputFailureCapability,
  type LocalOutputOperationFailureDiagnosticsPort,
} from '../../src/output/diagnostics'
import {
  type CompatibleNameActivationLedger,
} from '../../src/output/file-system-access/compatible-name/coordinator'
import {
  compatibleNameMappingV1,
  compatibleNameOperationBootstrapV1,
  compatibleNameOperationHeaderV1,
  compatibleNameRepairSummary,
  type CompatibleNameMappingV1,
  type CompatibleNameOperationBootstrapV1,
  type CompatibleNameOperationHeaderV1,
  type CompatibleNameOperationSnapshotV1,
  type CompatibleNamePendingTerminalOutcomeV1,
  type CompatibleNameRepairSummary,
} from '../../src/output/file-system-access/compatible-name/model'
import type { ReceiveOperationTransition } from '../../src/output/workspace/repository'
import { receiveOperationHandleRecord } from '../../src/output/workspace/records'
import {
  createSelectionSpec,
  type ArtifactChoiceID,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  FSAArtifactPresentationAuthority,
  type FSARouteDependencies,
} from '../../src/ui/browser-receive/fsa-route'
import {
  MemoryDirectory,
  MemoryLockManager,
  MemoryOperationRepository,
  memoryCheckpointFactory,
} from '../output/file-system-access-lifecycle-fixture'
import {
  COMPLETE_DISCOVERY,
  environment,
  fsaTarget,
  identity,
  projection,
  treeProof,
} from '../output/planning/fixture'


export interface PlanningFixture {
  readonly selection: Awaited<ReturnType<typeof selectionSpec>>
  readonly offered: OfferedArtifactChoice
  readonly action: ResolvedArtifactAction
}

export async function planningFixture(): Promise<PlanningFixture> {
  const selection = await selectionSpec()
  const currentProjection = projection(selection, treeProof(), 10n)
  const currentEnvironment = environment({ targets: [fsaTarget('fsa-route')] })
  const offers = await offerArtifacts(currentProjection, COMPLETE_DISCOVERY, currentEnvironment)
  if (offers.kind !== 'artifact-actions') throw new Error('expected an offered FSA choice')
  const offered = offers.primary
  const outcome = await reconcileArtifactChoice({
    choice: offered.choice,
    preferredRoute: materializationRouteIdentity(offered.route),
    expectedSelectionDigest: selection.digest,
    projection: currentProjection,
    discovery: COMPLETE_DISCOVERY,
    environment: currentEnvironment,
    previousObservation: {
      projectionEpoch: currentProjection.epoch,
      selectionDigest: selection.digest,
      resolvedArtifactDigest: null,
    },
  })
  if (outcome.kind !== 'resolved') throw new Error(`expected resolution, received ${outcome.kind}`)
  return Object.freeze({ selection, offered, action: outcome.action })
}

export async function replacementResolvedAction(
  planning: PlanningFixture,
): Promise<ResolvedArtifactAction> {
  const replacementProjection = projection(
    planning.selection,
    treeProof({
      kind: 'directory-selection',
      anchor: { directoryId: identity(31), sourcePath: 'photos-refined' },
    }),
    20n,
    2n,
  )
  const replacementEnvironment = environment({ targets: [fsaTarget('fsa-route')] })
  const outcome = await reconcileArtifactChoice({
    choice: planning.offered.choice,
    preferredRoute: materializationRouteIdentity(planning.offered.route),
    expectedSelectionDigest: planning.selection.digest,
    projection: replacementProjection,
    discovery: COMPLETE_DISCOVERY,
    environment: replacementEnvironment,
    previousObservation: {
      projectionEpoch: planning.action.projectionEpoch,
      selectionDigest: planning.action.selectionDigest,
      resolvedArtifactDigest: planning.action.resolvedArtifactDigest,
    },
  })
  if (outcome.kind !== 'resolved') {
    throw new Error(`expected replacement resolution, received ${outcome.kind}`)
  }
  return outcome.action
}

export function routeFixture(
  offered: OfferedArtifactChoice,
  picked: Promise<AcquiredFSAParentAuthority>,
  _parent: MemoryDirectory,
  repository: TestRepository,
  overrides: Partial<FSARouteDependencies> = {},
  localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort,
  preClickRanking: readonly ArtifactChoiceID[] = [offered.choice.choiceId],
): FSAArtifactPresentationAuthority {
  const operationLocks = new MemoryLockManager()
  const diagnosticAttempt = localOutputFailures === undefined
    ? undefined
    : createAttemptOutputFailureCapability(
        createIncidentScopeIssuer().open('authority_activation').handle,
      )
  return new FSAArtifactPresentationAuthority({
    offered,
    picked,
    preClickRanking,
    dependencies: {
      openRepository: async () => repository,
      acquireOperationLease: (store, operationId, options) =>
        acquireBrowserReceiveOperationLease(store, operationId, {
          ...options,
          manager: operationLocks,
          clock: { now: () => 1_000 },
          randomBytes: length => new Uint8Array(length).fill(9),
        }),
      acquireRootLease: handle => acquireFSARootMutationLease(handle, new MemoryLockManager()),
      createOperationId: () => identity(40),
      createReservationId: () => identity(41),
      createAuthorityRef: () => identity(42, 32),
      createOutputSessionId: () => identity(44),
      createTransferJobId: () => identity(43),
      checkpointRepositoryFactory: memoryCheckpointFactory(),
      ...overrides,
    },
    ...(diagnosticAttempt === undefined
      ? {}
      : {
          diagnostics: {
            backend: 'file_system_access' as const,
            failures: diagnosticAttempt.sinks,
          },
        }),
    ...(localOutputFailures === undefined ? {} : { localOutputFailures }),
  })
}

export function commitInput(planning: PlanningFixture) {
  return commitInputForAction(planning.selection, planning.action)
}

export function commitInputForAction(
  selection: PlanningFixture['selection'],
  action: ResolvedArtifactAction,
) {
  return {
    action,
    signal: new AbortController().signal,
    freezeAtFence: (candidate: Parameters<typeof bindReceiveIntent>[0]['candidate']) =>
      bindReceiveIntent({ selection, action, candidate }),
  }
}

export function acquiredParent(
  parent: MemoryDirectory,
  offered: OfferedArtifactChoice,
): AcquiredFSAParentAuthority {
  if (offered.route.kind !== 'direct-tree' ||
      offered.route.target.kind !== 'fsa-parent-directory') throw new Error('expected FSA route')
  return Object.freeze({
    kind: 'fsa-parent-directory-authority',
    targetRouteId: offered.route.target.routeId,
    offer: fsaParentOffer(offered.route.target.routeId),
    parent: parent as unknown as FileSystemDirectoryHandle,
  })
}

export async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

export function requireDirectTreeIntent(intent: ReceiveIntent): ReceiveIntent & Readonly<{ plan: DirectTreePlan }> {
  if (intent.plan.kind !== 'direct-tree') throw new Error('expected DirectTree intent')
  return intent as ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
}

export function fsaReservationName(intent: ReceiveIntent & Readonly<{ plan: DirectTreePlan }>): string {
  const reservation = intent.plan.reservation
  if (reservation.kind !== 'named-container-entry' || reservation.authorityKind !== 'fsa-container') {
    throw new Error('expected an FSA named-entry reservation')
  }
  return reservation.physicalName
}

export function requireDirectRoute(action: ResolvedArtifactAction) {
  if (action.route.kind !== 'direct-tree') throw new Error('expected DirectTree route')
  return action.route
}

export function classifiedRootRejection(
  component: string,
  expectedKind: 'file' | 'directory',
): PathComponentRejectedError {
  return new PathComponentRejectedError({
    cause: new TypeError('injected native root refusal'),
    canonicalComponent: component,
    expectedKind,
    stage: 'fsa.root.entry.inspect',
  })
}

export class TestRepository extends MemoryOperationRepository {
  readonly transitions: ReceiveOperationTransition[] = []
  compatibleBootstrap: CompatibleNameOperationBootstrapV1 | undefined
  compatibleBootstrapCommits = 0
  closeCount = 0
  failNextTransition = false
  hideNextCommittedLifecycle = false
  afterNextTransition: (() => void) | undefined

  override async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    if (this.failNextTransition) {
      this.failNextTransition = false
      throw new DOMException('injected transaction abort', 'AbortError')
    }
    this.transitions.push(transition)
    await super.commitTransition(transition)
    const afterTransition = this.afterNextTransition
    this.afterNextTransition = undefined
    afterTransition?.()
  }

  async commitFSACompatibleNameBootstrap(input: Readonly<{
    transition: ReceiveOperationTransition
    bootstrap: CompatibleNameOperationBootstrapV1
  }>): Promise<void> {
    const bootstrap = compatibleNameOperationBootstrapV1(input.bootstrap)
    await this.commitTransition(input.transition)
    this.compatibleBootstrap = bootstrap
    this.compatibleBootstrapCommits += 1
  }

  override close(): void {
    this.closeCount += 1
  }

  override async readLifecycle(operationId: string) {
    if (this.hideNextCommittedLifecycle && this.transitions.length !== 0) {
      this.hideNextCommittedLifecycle = false
      return undefined
    }
    return super.readLifecycle(operationId)
  }
}

export class MemoryCompatibleNameActivationLedger implements CompatibleNameActivationLedger {
  header: CompatibleNameOperationHeaderV1 | undefined
  mapping: CompatibleNameMappingV1 | undefined
  closeCount = 0
  readonly #repository: TestRepository

  constructor(repository: TestRepository) {
    this.#repository = repository
  }

  async readHeader(operationId: string): Promise<CompatibleNameOperationHeaderV1 | undefined> {
    const snapshot = await this.loadOperation(operationId)
    return snapshot?.header
  }

  async bootstrapOperation(input: Parameters<
    CompatibleNameActivationLedger['bootstrapOperation']
  >[0]): Promise<CompatibleNameOperationSnapshotV1> {
    const bootstrap = compatibleNameOperationBootstrapV1(input)
    this.header = bootstrap.header
    this.mapping = bootstrap.initialMapping
    return Object.freeze({ header: this.header, mappings: Object.freeze([this.mapping]) })
  }

  async claimMapping(input: Parameters<
    CompatibleNameActivationLedger['claimMapping']
  >[0]): Promise<CompatibleNameMappingV1> {
    this.mapping = compatibleNameMappingV1(input)
    return this.mapping
  }

  async scanCommittedMappings(
    operationId: string,
    afterOrdinal = 0,
  ): Promise<readonly CompatibleNameMappingV1[]> {
    const mapping = this.#requireMapping(operationId)
    return mapping.commitOrdinal !== undefined && mapping.commitOrdinal > afterOrdinal
      ? Object.freeze([mapping])
      : Object.freeze([])
  }

  async loadOperation(operationId: string): Promise<CompatibleNameOperationSnapshotV1 | undefined> {
    const bootstrap = this.#repository.compatibleBootstrap
    if (bootstrap === undefined || bootstrap.header.operationId !== operationId) return undefined
    this.header ??= bootstrap.header
    this.mapping ??= bootstrap.initialMapping
    return Object.freeze({ header: this.header, mappings: Object.freeze([this.mapping]) })
  }

  async recordPairOwnership(input: Parameters<
    CompatibleNameActivationLedger['recordPairOwnership']
  >[0]): Promise<CompatibleNameOperationHeaderV1> {
    const header = this.#requireHeader(input.operationId)
    if (input.handle.kind !== 'file') throw new TypeError('pair fixture requires a file handle')
    const identity = header.pair[input.pairKind]
    await this.#repository.commitTransition({
      operationId: input.operationId,
      handles: [receiveOperationHandleRecord({
        id: identity.handleId,
        operationId: input.operationId,
        kind: input.pairKind === 'script'
          ? FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT
          : FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR,
        authorityRef: header.authorityRef,
        ownedObjectId: identity.ownedObjectId,
        handle: input.handle,
      })],
    })
    const pair = Object.freeze({
      ...header.pair,
      [input.pairKind]: Object.freeze({
        ...header.pair[input.pairKind],
        ownershipState: 'owned' as const,
      }),
    })
    const pairReady = pair.script.ownershipState === 'owned' && pair.sidecar.ownershipState === 'owned'
    this.header = compatibleNameOperationHeaderV1({
      ...header,
      pair,
      activationState: pairReady ? 'pair-ready' : 'prepared',
    })
    return this.header
  }

  async recordCompatibleTargetCreated(input: Parameters<
    CompatibleNameActivationLedger['recordCompatibleTargetCreated']
  >[0]): Promise<CompatibleNameOperationHeaderV1> {
    const header = this.#requireHeader(input.operationId)
    this.#requireMapping(input.operationId)
    if (header.activationState === 'active') return header
    this.header = compatibleNameOperationHeaderV1({
      ...header,
      activationState: 'active',
      repairSummary: input.repairSummary,
    })
    return this.header
  }

  async recordVerifiedDirectoryOwnership(input: Parameters<
    CompatibleNameActivationLedger['recordVerifiedDirectoryOwnership']
  >[0]): Promise<CompatibleNameMappingV1> {
    const mapping = this.#requireMapping(input.operationId)
    this.mapping = compatibleNameMappingV1({
      ...mapping,
      ownershipState: 'owned',
      ownedObjectId: input.ownedObjectId,
    })
    return this.mapping
  }

  async commitMapping(input: Parameters<
    CompatibleNameActivationLedger['commitMapping']
  >[0]): Promise<CompatibleNameMappingV1> {
    const mapping = this.#requireMapping(input.operationId)
    if (mapping.ownershipState !== 'owned' || mapping.ownedObjectId !== input.ownedObjectId) {
      throw new TypeError('mapping fixture lost root ownership')
    }
    this.mapping = compatibleNameMappingV1({
      ...mapping,
      commitState: 'committed',
      commitOrdinal: 1,
    })
    this.header = compatibleNameOperationHeaderV1({
      ...this.#requireHeader(input.operationId),
      ...(this.header?.repairSummary === undefined
        ? {}
        : {
            repairSummary: compatibleNameRepairSummary({
              ...this.header.repairSummary,
              committedCount: 1,
              logicalPathSample: [this.mapping.logicalPath],
              pendingCatchUp: true,
            }),
          }),
    })
    return this.mapping
  }

  async persistRepairSummary(
    operationId: string,
    summary: CompatibleNameRepairSummary,
  ): Promise<CompatibleNameOperationHeaderV1> {
    this.header = compatibleNameOperationHeaderV1({
      ...this.#requireHeader(operationId),
      repairSummary: compatibleNameRepairSummary(summary),
    })
    return this.header
  }

  async persistPendingTerminalOutcome(input: {
    operationId: string
    outcome: CompatibleNamePendingTerminalOutcomeV1
    repairSummary: CompatibleNameRepairSummary
  }): Promise<CompatibleNameOperationHeaderV1> {
    this.header = compatibleNameOperationHeaderV1({
      ...this.#requireHeader(input.operationId),
      pendingTerminalOutcome: input.outcome,
      repairSummary: input.repairSummary,
    })
    return this.header
  }

  async readPendingTerminalOutcome(
    operationId: string,
  ): Promise<CompatibleNamePendingTerminalOutcomeV1 | undefined> {
    return this.#requireHeader(operationId).pendingTerminalOutcome
  }

  async clearPendingTerminalOutcome(input: {
    operationId: string
    repairSummary: CompatibleNameRepairSummary
  }): Promise<CompatibleNameOperationHeaderV1> {
    const header = { ...this.#requireHeader(input.operationId), repairSummary: input.repairSummary }
    delete header.pendingTerminalOutcome
    this.header = compatibleNameOperationHeaderV1(header)
    return this.header
  }

  async readRepairSummary(operationId: string): Promise<CompatibleNameRepairSummary | undefined> {
    return this.#requireHeader(operationId).repairSummary
  }

  async removeVerifiedEmptyOperation(
    expectedHeader: CompatibleNameOperationHeaderV1,
  ): Promise<void> {
    if (this.header === undefined) return
    if (this.header.operationId !== expectedHeader.operationId ||
        this.mapping?.commitState === 'committed') {
      throw new Error('compatible repair fixture is not empty')
    }
    await this.#repository.commitTransition({
      operationId: expectedHeader.operationId,
      deleteHandleIds: [expectedHeader.pair.script.handleId, expectedHeader.pair.sidecar.handleId],
    })
    this.header = undefined
    this.mapping = undefined
  }

  close(): void {
    this.closeCount += 1
  }

  #requireHeader(operationId: string): CompatibleNameOperationHeaderV1 {
    if (this.header?.operationId !== operationId) throw new Error('compatible header is missing')
    return this.header
  }

  #requireMapping(operationId: string): CompatibleNameMappingV1 {
    if (this.mapping?.operationId !== operationId) throw new Error('compatible mapping is missing')
    return this.mapping
  }
}

export function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}
