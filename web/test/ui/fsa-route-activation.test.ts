import { describe, expect, it } from 'vitest'

import { acquireFSARootMutationLease } from '../../src/output/browser/namespace-mutation'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { authorizeFSAParent, fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../src/output/planning'
import type { FSAFileCheckpointRepositoryFactory } from '../../src/output/file-system-access/session'
import type { ReceiveOperationTransition } from '../../src/output/workspace/repository'
import {
  createSelectionSpec,
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

describe('FSA presentation route activation', () => {
  it('starts no route work while the one picker is pending and drains late success after release', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const picked = deferred<AcquiredFSAParentAuthority>()
    const observed: string[] = []
    const route = routeFixture(planning.offered, picked.promise, parent, new TestRepository(), {
      authorizeParent: async (authority) => {
        observed.push('authorize')
        await authorizeFSAParent(authority)
      },
      acquireRootLease: async (handle) => {
        observed.push('root-lease')
        return acquireFSARootMutationLease(handle, new MemoryLockManager())
      },
    })

    const commit = route.commit(commitInput(planning)).catch(error => error)
    await Promise.resolve()
    expect(observed).toEqual([])
    route.release(new DOMException('replaced', 'AbortError'))
    picked.resolve(acquiredParent(parent, planning.offered))

    await expect(route.ready).resolves.toBeUndefined()
    await expect(commit).resolves.toBeInstanceOf(Error)
    expect(observed).toEqual([])
  })

  it('drains a late picker refusal after release without opening route resources', async () => {
    const planning = await planningFixture()
    const picked = deferred<AcquiredFSAParentAuthority>()
    const repository = new TestRepository()
    const route = routeFixture(
      planning.offered,
      picked.promise,
      new MemoryDirectory('downloads'),
      repository,
    )
    route.release(new DOMException('cancelled', 'AbortError'))
    picked.reject(new DOMException('picker refused', 'AbortError'))

    await expect(route.ready).rejects.toMatchObject({ outcome: 'picker_refused' })
    expect(repository.transitions).toEqual([])
    expect(repository.closeCount).toBe(0)
  })

  it('authorizes before acquiring the root lease and keeps the namespace absent through bound return', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const order: string[] = []
    const locks = new MemoryLockManager()
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        authorizeParent: async (authority) => {
          order.push('authorize')
          await authorizeFSAParent(authority)
        },
        acquireRootLease: async (handle) => {
          order.push('root-lease')
          return acquireFSARootMutationLease(handle, locks)
        },
      },
    )

    await route.ready
    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    expect(order).toEqual(['authorize', 'root-lease'])
    expect(repository.transitions[0]).toMatchObject({
      operationId: result.operation.intent.operationId,
      records: expect.arrayContaining([expect.objectContaining({ operationId: result.operation.intent.operationId })]),
      handles: [expect.objectContaining({ operationId: result.operation.intent.operationId })],
      lifecycle: expect.objectContaining({ kind: 'intent-frozen' }),
      lease: expect.objectContaining({ kind: 'put' }),
    })
    expect(parent.entryNames()).toEqual([])

    const directIntent = requireDirectTreeIntent(result.operation.intent)
    await result.operation.plans.openDirectTree(directIntent, new AbortController().signal)
    expect(parent.directoryNames()).toEqual([fsaReservationName(directIntent)])
    await result.operation.detach()
  })

  it('reuses the settled picker authority after clean cancellation before candidate preparation', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const firstAttempt = new AbortController()
    let pickerSettlementCount = 0
    let authorizationCount = 0
    const picked = Promise.resolve(acquiredParent(parent, planning.offered)).then((authority) => {
      pickerSettlementCount += 1
      return authority
    })
    const route = routeFixture(planning.offered, picked, parent, repository, {
      authorizeParent: async (authority) => {
        authorizationCount += 1
        await authorizeFSAParent(authority)
        if (authorizationCount === 1) {
          firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
        }
      },
    })

    await route.ready
    await expect(route.commit({
      ...commitInput(planning),
      signal: firstAttempt.signal,
    })).resolves.toEqual({ kind: 'retryable-precut' })
    expect(repository.transitions).toEqual([])
    expect(repository.closeCount).toBe(0)

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    expect(pickerSettlementCount).toBe(1)
    expect(authorizationCount).toBe(2)
    await result.operation.detach()
  })

  it('recommits with the newest resolved artifact after the final fence cancels cleanly', async () => {
    const planning = await planningFixture()
    const replacementAction = await replacementResolvedAction(planning)
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const rootLocks = new MemoryLockManager()
    const firstAttempt = new AbortController()
    let pickerSettlementCount = 0
    const picked = Promise.resolve(acquiredParent(parent, planning.offered)).then((authority) => {
      pickerSettlementCount += 1
      return authority
    })
    const route = routeFixture(planning.offered, picked, parent, repository, {
      acquireRootLease: handle => acquireFSARootMutationLease(handle, rootLocks),
    })

    const firstResult = await route.commit({
      action: planning.action,
      signal: firstAttempt.signal,
      freezeAtFence: async (candidate) => {
        await bindReceiveIntent({ selection: planning.selection, action: planning.action, candidate })
        firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
        firstAttempt.signal.throwIfAborted()
        throw new Error('unreachable final fence')
      },
    })
    expect(firstResult).toEqual({
      kind: 'retryable-precut',
      receiverOperationId: identity(40),
    })
    expect(repository.transitions).toEqual([])
    expect(repository.closeCount).toBe(1)
    expect(rootLocks.releaseCount).toBe(1)

    const result = await route.commit(commitInputForAction(planning.selection, replacementAction))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    expect(replacementAction.artifact.digest).not.toBe(planning.action.artifact.digest)
    expect(result.operation.intent.artifact.digest).toBe(replacementAction.artifact.digest)
    expect(pickerSettlementCount).toBe(1)
    await result.operation.detach()
  })

  it('recommits after cancellation between the final fence and the durable cut', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const operationLocks = new MemoryLockManager()
    const firstAttempt = new AbortController()
    const leaseEntered = deferred<void>()
    const releaseLeaseAttempt = deferred<void>()
    let leaseAttemptCount = 0
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        acquireOperationLease: async (store, operationId, options) => {
          leaseAttemptCount += 1
          if (leaseAttemptCount === 1) {
            leaseEntered.resolve()
            await releaseLeaseAttempt.promise
            firstAttempt.signal.throwIfAborted()
          }
          return acquireBrowserReceiveOperationLease(store, operationId, {
            ...options,
            manager: operationLocks,
            clock: { now: () => 1_000 },
            randomBytes: length => new Uint8Array(length).fill(9),
          })
        },
      },
    )

    const firstCommit = route.commit({ ...commitInput(planning), signal: firstAttempt.signal })
    await leaseEntered.promise
    firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
    releaseLeaseAttempt.resolve()
    await expect(firstCommit).resolves.toEqual({
      kind: 'retryable-precut',
      receiverOperationId: identity(40),
    })
    expect(repository.records.size).toBe(0)
    expect(repository.handles.size).toBe(0)
    expect(repository.leases.size).toBe(0)

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    expect(leaseAttemptCount).toBe(2)
    await result.operation.detach()
  })
})

describe('FSA presentation route failure ownership', () => {
  it('releases every transient authority when the final fence invalidates', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const rootLocks = new MemoryLockManager()
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        acquireRootLease: handle => acquireFSARootMutationLease(handle, rootLocks),
      },
    )

    await expect(route.commit({
      action: planning.action,
      signal: new AbortController().signal,
      freezeAtFence: async () => {
        throw new DOMException('selection changed', 'AbortError')
      },
    })).rejects.toThrow('selection changed')
    expect(repository.records.size).toBe(0)
    expect(repository.handles.size).toBe(0)
    expect(repository.leases.size).toBe(0)
    expect(repository.closeCount).toBe(1)
    expect(rootLocks.releaseCount).toBe(1)
    expect(parent.entryNames()).toEqual([])
  })

  it('rejects an exact route-ID mismatch before authorization despite compatible guarantees', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    let authorizationCount = 0
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      new TestRepository(),
      { authorizeParent: async () => { authorizationCount += 1 } },
    )
    const mismatched = Object.freeze({
      ...planning.action,
      route: Object.freeze({
        ...planning.action.route,
        target: Object.freeze({ ...requireDirectRoute(planning.action).target, routeId: 'other-fsa' }),
      }),
    }) as ResolvedArtifactAction

    await expect(route.commit({ ...commitInput(planning), action: mismatched })).rejects.toThrow(
      /installed FSA DirectTree route/u,
    )
    expect(authorizationCount).toBe(0)
  })

  it('rejects picker authority whose facts do not exactly match the installed route', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const authority = acquiredParent(parent, planning.offered)
    const mismatchedAuthority: AcquiredFSAParentAuthority = Object.freeze({
      ...authority,
      offer: Object.freeze({ ...authority.offer, hardMaximumOutputBytes: 1n }),
    })
    let authorizationCount = 0
    const route = routeFixture(
      planning.offered,
      Promise.resolve(mismatchedAuthority),
      parent,
      new TestRepository(),
      { authorizeParent: async () => { authorizationCount += 1 } },
    )

    await expect(route.ready).rejects.toThrow(/installed route identity/u)
    await expect(route.commit(commitInput(planning))).rejects.toThrow(/installed route identity/u)
    expect(authorizationCount).toBe(0)
  })

  it('treats an aborted atomic transition as pre-cut and stores nothing', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    repository.failNextTransition = true
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
    )

    await expect(route.commit(commitInput(planning))).rejects.toThrow('injected transaction abort')
    expect(repository.records.size).toBe(0)
    expect(repository.handles.size).toBe(0)
    expect(repository.leases.size).toBe(0)
    expect(parent.entryNames()).toEqual([])
    await expect(route.commit(commitInput(planning))).rejects.toThrow(/no longer available/u)
  })

  it('returns owned effects for post-cut assembly failure and settles the unopened operation', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const failingCheckpoints: FSAFileCheckpointRepositoryFactory = async () => {
      throw new DOMException('checkpoint repository unavailable', 'UnknownError')
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      { checkpointRepositoryFactory: failingCheckpoints },
    )

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    expect(repository.records.size).toBeGreaterThan(0)
    expect(parent.entryNames()).toEqual([])
    const settled = await result.authority.settleActivationFailure(result.cause)
    expect(settled.lifecycle.kind).toBe('discarded')
    await result.authority.detach()
    await result.authority.detach()
    expect(repository.leases.size).toBe(0)
    expect(repository.closeCount).toBe(1)
  })

  it('keeps ownership when cancellation lands as the atomic durable transition commits', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const attempt = new AbortController()
    repository.afterNextTransition = () => {
      attempt.abort(new DOMException('projection replacement', 'AbortError'))
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
    )

    const result = await route.commit({ ...commitInput(planning), signal: attempt.signal })
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    expect(repository.records.size).toBeGreaterThan(0)
    expect(repository.leases.size).toBe(1)
    await expect(result.authority.settleActivationFailure(result.cause)).resolves.toMatchObject({
      lifecycle: { kind: 'discarded' },
    })
    await result.authority.detach()
    expect(repository.leases.size).toBe(0)
  })

  it('keeps post-cut ownership when cancellation arrives before the bound return', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const attempt = new AbortController()
    const checkpointsEntered = deferred<void>()
    const continueCheckpoints = deferred<void>()
    const openCheckpoints = memoryCheckpointFactory()
    const delayedCheckpoints: FSAFileCheckpointRepositoryFactory = async (binding) => {
      checkpointsEntered.resolve()
      await continueCheckpoints.promise
      return openCheckpoints(binding)
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      { checkpointRepositoryFactory: delayedCheckpoints },
    )

    const commit = route.commit({ ...commitInput(planning), signal: attempt.signal })
    await checkpointsEntered.promise
    attempt.abort(new DOMException('selection changed', 'AbortError'))
    continueCheckpoints.resolve()
    const result = await commit
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    expect(repository.records.size).toBeGreaterThan(0)
    await expect(result.authority.settleActivationFailure(result.cause)).resolves.toMatchObject({
      lifecycle: { kind: 'discarded' },
    })
    await result.authority.detach()
    expect(repository.leases.size).toBe(0)
  })

  it('persists NeedsAttention when post-cut owner verification is inconclusive', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    repository.hideNextCommittedLifecycle = true
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
    )

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    await expect(result.authority.settleActivationFailure(result.cause)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'target-ownership-unknown' },
    })
    await result.authority.detach()
    expect(parent.entryNames()).toEqual([])
  })
})

interface PlanningFixture {
  readonly selection: Awaited<ReturnType<typeof selectionSpec>>
  readonly offered: OfferedArtifactChoice
  readonly action: ResolvedArtifactAction
}

async function planningFixture(): Promise<PlanningFixture> {
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

async function replacementResolvedAction(
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

function routeFixture(
  offered: OfferedArtifactChoice,
  picked: Promise<AcquiredFSAParentAuthority>,
  _parent: MemoryDirectory,
  repository: TestRepository,
  overrides: Partial<FSARouteDependencies> = {},
): FSAArtifactPresentationAuthority {
  const operationLocks = new MemoryLockManager()
  return new FSAArtifactPresentationAuthority({
    offered,
    picked,
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
      createTransferJobId: () => identity(43),
      checkpointRepositoryFactory: memoryCheckpointFactory(),
      ...overrides,
    },
  })
}

function commitInput(planning: PlanningFixture) {
  return commitInputForAction(planning.selection, planning.action)
}

function commitInputForAction(
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

function acquiredParent(
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

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

function requireDirectTreeIntent(intent: ReceiveIntent): ReceiveIntent & Readonly<{ plan: DirectTreePlan }> {
  if (intent.plan.kind !== 'direct-tree') throw new Error('expected DirectTree intent')
  return intent as ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
}

function fsaReservationName(intent: ReceiveIntent & Readonly<{ plan: DirectTreePlan }>): string {
  const reservation = intent.plan.reservation
  if (reservation.kind !== 'named-container-entry' || reservation.authorityKind !== 'fsa-container') {
    throw new Error('expected an FSA named-entry reservation')
  }
  return reservation.reservedName
}

function requireDirectRoute(action: ResolvedArtifactAction) {
  if (action.route.kind !== 'direct-tree') throw new Error('expected DirectTree route')
  return action.route
}

class TestRepository extends MemoryOperationRepository {
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

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}
