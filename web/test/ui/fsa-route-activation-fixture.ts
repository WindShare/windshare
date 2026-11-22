import { acquireFSARootMutationLease } from '../output/fsa-mutation-lock-fixture'
import { PathComponentRejectedError } from '../../src/output/browser/filesystem-component-inspection'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { createIncidentScopeIssuer } from '../../src/diagnostics/incident'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import { bindReceiveIntent } from '../../src/output/planning/binding'
import { materializationRouteIdentity } from '../../src/output/planning/guarantees'
import { offerArtifacts } from '../../src/output/planning/offers'
import { reconcileArtifactChoice } from '../../src/output/planning/reconciliation'
import { type OfferedArtifactChoice } from '../../src/output/model/artifacts/choices'
import { type ResolvedArtifactAction } from '../../src/output/planning/contracts'
import {
  createAttemptOutputFailureCapability,
  type LocalOutputOperationFailureDiagnosticsPort,
} from '../../src/output/diagnostics'
import type { ReceiveOperationTransition } from '../../src/output/workspace/repository'
import {
  createSelectionSpec,
  type ArtifactChoiceID,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  FSAArtifactPresentationAuthority,
  type FSARouteDependencies,
} from '../../src/application/browser-receive/fsa/route'
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
    evidence: {
      kind: 'non-creating-lookup',
      stage: 'fsa.root.entry.inspect',
      cause: new TypeError('injected native root refusal'),
    },
    canonicalComponent: component,
    expectedKind,
  })
}

export class TestRepository extends MemoryOperationRepository {
  readonly transitions: ReceiveOperationTransition[] = []
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

export function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}
