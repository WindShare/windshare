import { describe, expect, it, vi } from 'vitest'

import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type CandidateMaterializationBinding,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../src/output/planning'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_WORKSPACE_ACTIVATION,
  createWorkspaceActivationCandidate,
  decodeStoredWorkspaceActivationCandidate,
  receiveOperationLeaseRecord,
  type ManifestPageRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
  type ReceiveRecordKind,
} from '../../src/output/workspace/records'
import {
  prepareReceiveOperationTransition,
  type ReceiveOperationTransition,
  type WorkspaceActivationJournalRepository,
} from '../../src/output/workspace/repository'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { recoverWorkspaceActivationCandidate } from '../../src/output/workspace/activation-recovery'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
} from '../../src/output/workspace/state'
import {
  journalWorkspaceActivation,
  promoteWorkspaceActivation,
  WorkspaceOperationStages,
} from '../../src/output/workspace/stages'
import {
  createSelectionSpec,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import type { V2PlanExecutionAuthority } from '../../src/transfer/output-session'
import type {
  V2BoundReceiveOperation,
  V2RouteCommitResult,
} from '../../src/ui/v2-receive-runtime'
import type { BrowserReceiveWindow } from '../../src/ui/browser-receive/contracts'
import {
  WorkspaceArtifactPresentationAuthority,
  type WorkspaceRouteDependencyOverrides,
} from '../../src/ui/browser-receive/workspace-route'
import {
  OriginPrivateWorkspaceNamespaceOpenError,
  type OriginPrivateWorkspaceNamespace,
} from '../../src/output/origin-private/namespace'
import {
  COMPLETE_DISCOVERY,
  environment,
  handoffTarget,
  identity,
  projection,
  singleFileProof,
  workspaceOffer,
} from '../output/planning/fixture'

const OPERATION_ID = identity(80)
const WORKSPACE_ID = identity(81)
const REPOSITORY_REFERENCE = identity(82, 32)
const LEASE_ID = identity(83)

describe('workspace route activation ownership', () => {
  it('creates no repository or namespace effects before the final fence succeeds', async () => {
    const fixture = await routeFixture()
    const authority = fixture.authority()

    await expect(authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: async () => { throw new DOMException('stale selection', 'AbortError') },
    })).rejects.toThrow('stale selection')

    expect(fixture.events).toEqual([])
    expect(fixture.repository.records(OPERATION_ID)).toEqual([])
    expect(fixture.parent.hasNamespace).toBe(false)
  })

  it('returns one bound operation only after persistence, lease, and runtime assembly', async () => {
    const fixture = await routeFixture()
    const result = await fixture.commit()

    expect(result.kind).toBe('bound-operation')
    expect(fixture.events).toEqual([
      'fence',
      'repository-open',
      'namespace-create',
      'namespace-persisted',
      'lease-acquired',
      'stages-opened',
      'runtime-created',
    ])
    expect(fixture.repository.records(OPERATION_ID)).not.toEqual([])
    expect(fixture.parent.hasNamespace).toBe(true)
    expect(await fixture.repository.readLease(OPERATION_ID)).toEqual(expect.objectContaining({
      leaseId: LEASE_ID,
    }))
  })

  it('serializes activation recovery until promotion and the persistent lease are complete', async () => {
    const fixture = await routeFixture({ failure: 'during-runtime', trackActivationLock: true })
    const owned = requireOwned(await fixture.commit())

    expect(fixture.events).toEqual([
      'fence',
      'repository-open',
      'activation-lock-enter',
      'namespace-create',
      'namespace-persisted',
      'lease-acquired',
      'activation-lock-exit',
      'stages-opened',
    ])

    await owned.authority.settleActivationFailure(owned.cause)
    expect(fixture.events.filter(event => event === 'activation-lock-enter')).toHaveLength(2)
    expect(fixture.events.at(-1)).toBe('activation-lock-exit')
  })

  it('rejects a repository-open activation only after proving both stores absent', async () => {
    const fixture = await routeFixture({ failure: 'before-namespace' })

    await expect(fixture.commit()).rejects.toThrow('namespace unavailable')
    expect(fixture.repository.closed).toBe(true)
    expect(fixture.repository.records(OPERATION_ID)).toEqual([])
    expect(fixture.parent.hasNamespace).toBe(false)
    await expect(fixture.repository.readLease(OPERATION_ID)).resolves.toBeUndefined()
  })

  it.each([
    'after-persistence',
    'after-persistence-without-callback',
    'after-lease',
    'during-runtime',
    'abort-after-persistence',
  ] as const)('returns owned effects for a failure %s and settles them as discarded', async (failure) => {
    const fixture = await routeFixture({ failure })
    const result = await fixture.commit()
    const owned = requireOwned(result)

    await expect(owned.authority.settleActivationFailure(owned.cause)).resolves.toEqual({
      lifecycle: expect.objectContaining({ kind: 'discarded' }),
      workspaceUsage: null,
    })
    await owned.authority.detach()

    const lifecycle = await fixture.repository.readLifecycle(OPERATION_ID)
    expect(lifecycle === undefined ? undefined : decodeStoredReceiveLifecycleState(lifecycle).kind)
      .toBe('discarded')
    expect(fixture.parent.hasNamespace).toBe(false)
    await expect(fixture.repository.readLease(OPERATION_ID)).resolves.toBeUndefined()
  })

  it('retains ambiguous namespace ownership as NeedsAttention instead of deleting records', async () => {
    const fixture = await routeFixture({ failure: 'during-runtime' })
    const result = await fixture.commit()
    fixture.parent.replaceNamespace()
    const owned = requireOwned(result)

    await expect(owned.authority.settleActivationFailure(owned.cause)).resolves.toEqual({
      lifecycle: expect.objectContaining({
        kind: 'needs-attention',
        reason: 'cleanup-unknown',
      }),
      workspaceUsage: null,
    })
    await owned.authority.detach()

    expect(fixture.parent.hasNamespace).toBe(true)
    expect(fixture.repository.records(OPERATION_ID)).not.toEqual([])
  })

  it('durably adopts an uncommitted namespace when exact rollback cannot prove ownership', async () => {
    const fixture = await routeFixture({ failure: 'uncommitted-namespace-unknown' })
    const result = await fixture.commit()
    const owned = requireOwned(result)

    await expect(owned.authority.settleActivationFailure(owned.cause)).resolves.toEqual({
      lifecycle: expect.objectContaining({
        kind: 'needs-attention',
        reason: 'target-ownership-unknown',
      }),
      workspaceUsage: null,
    })
    await owned.authority.detach()

    expect(fixture.parent.hasNamespace).toBe(true)
    expect(fixture.repository.records(OPERATION_ID)).not.toEqual([])
  })

  it('commits the newest compatible action while retaining the installed route identity', async () => {
    const fixture = await routeFixture()
    const refreshed = await workspaceResolvedAction(9_999_999n)
    const freezeAtFence = vi.fn((candidate: CandidateMaterializationBinding) =>
      bindReceiveIntent({ selection: fixture.selection, action: refreshed.action, candidate }))

    const result = await fixture.authority().commit({
      action: refreshed.action,
      signal: new AbortController().signal,
      freezeAtFence,
    })

    expect(result.kind).toBe('bound-operation')
    expect(freezeAtFence).toHaveBeenCalledWith(expect.objectContaining({
      kind: 'workspace-binding',
      workspaceRouteId: 'workspace-route',
      publicationTargetRouteId: 'handoff-route',
    }))
  })

  it('recommits the same authority after a proven clean pre-cut replacement abort', async () => {
    const fixture = await routeFixture()
    const authority = fixture.authority()
    const firstAbort = new AbortController()
    const first = await authority.commit({
      action: fixture.action,
      signal: firstAbort.signal,
      freezeAtFence: async candidate => {
        firstAbort.abort('observation replacement')
        return bindReceiveIntent({ selection: fixture.selection, action: fixture.action, candidate })
      },
    })
    expect(first).toEqual({ kind: 'retryable-precut', receiverOperationId: OPERATION_ID })

    const second = await authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: candidate =>
        bindReceiveIntent({ selection: fixture.selection, action: fixture.action, candidate }),
    })
    expect(second).toEqual(expect.objectContaining({ kind: 'bound-operation' }))
    if (second.kind !== 'bound-operation') throw new Error('workspace recommit did not bind')
    expect(second.operation.intent.operationId).toBe(OPERATION_ID)
  })

  it('settles a conservative owner after later clean-absence proof and detaches idempotently', async () => {
    const fixture = await routeFixture({ failure: 'conservative-absence' })
    const owned = requireOwned(await fixture.commit())

    const first = await owned.authority.settleActivationFailure(owned.cause)
    const second = await owned.authority.settleActivationFailure(new Error('ignored retry reason'))
    expect(first).toEqual(expect.objectContaining({
      lifecycle: expect.objectContaining({ kind: 'discarded' }),
    }))
    expect(second).toBe(first)
    await owned.authority.detach()
    await owned.authority.detach()
    expect(fixture.repository.closeCount).toBe(1)
  })

  it('retains ownership when cleanup and NeedsAttention persistence fail, then retries', async () => {
    const fixture = await routeFixture({ failure: 'during-runtime' })
    const owned = requireOwned(await fixture.commit())
    fixture.parent.replaceNamespace()
    fixture.repository.rejectNextNeedsAttentionTransitions(2)

    await expect(owned.authority.settleActivationFailure(owned.cause)).rejects.toThrow(
      'could not record its retained ownership state',
    )
    expect(fixture.repository.closeCount).toBe(0)
    await expect(owned.authority.settleActivationFailure(owned.cause)).resolves.toEqual({
      lifecycle: expect.objectContaining({ kind: 'needs-attention' }),
      workspaceUsage: null,
    })
    await owned.authority.detach()
    expect(fixture.repository.closeCount).toBe(1)
  })

  it('retries detach without repeating a successful settlement', async () => {
    const fixture = await routeFixture({ failure: 'conservative-absence' })
    const owned = requireOwned(await fixture.commit())
    const settlement = await owned.authority.settleActivationFailure(owned.cause)
    fixture.repository.rejectNextClose()

    await expect(owned.authority.detach()).rejects.toThrow('did not detach')
    expect(await owned.authority.settleActivationFailure(owned.cause)).toBe(settlement)
    await expect(owned.authority.detach()).resolves.toBeUndefined()
    expect(fixture.repository.closeCount).toBe(2)
  })
})

type FailurePoint =
  | 'before-namespace'
  | 'after-persistence'
  | 'after-persistence-without-callback'
  | 'after-lease'
  | 'during-runtime'
  | 'abort-after-persistence'
  | 'uncommitted-namespace-unknown'
  | 'conservative-absence'

async function routeFixture(options: {
  readonly failure?: FailurePoint
  readonly trackActivationLock?: boolean
} = {}) {
  const planned = await workspaceResolvedAction()
  const repository = new MemoryReceiveOperationRepository()
  const parent = new MemoryDirectoryHandle('root')
  const entryIdentity = identity(85, 32)
  const entryName = `activation-${entryIdentity}`
  const root = new MemoryDirectoryHandle(entryName)
  const namespace: OriginPrivateWorkspaceNamespace = Object.freeze({
    operationId: OPERATION_ID,
    authorityRef: REPOSITORY_REFERENCE,
    parent: parent as unknown as FileSystemDirectoryHandle,
    entryName,
    root: root as unknown as FileSystemDirectoryHandle,
    rootHandleId: `windshare/origin-private/workspace-root/v1/${OPERATION_ID}`,
    rootOwnedObjectId: identity(84, 32),
  })
  root.installMarker(
    `windshare/workspace-activation-owner/v1\n${OPERATION_ID}\n${namespace.rootOwnedObjectId}\n${REPOSITORY_REFERENCE}\n`,
  )
  const activationCandidate = await createWorkspaceActivationCandidate({
    operationId: OPERATION_ID,
    entryIdentity,
    rootHandleId: namespace.rootHandleId,
    rootOwnedObjectId: namespace.rootOwnedObjectId,
    repositoryAuthority: REPOSITORY_REFERENCE,
  })
  const events: string[] = []
  const abort = new AbortController()
  const boundOperations: V2BoundReceiveOperation[] = []
  const dependencies: WorkspaceRouteDependencyOverrides = {
    activationOwner: {
      withActivationLock: async (_operationId, execute) => {
        if (options.trackActivationLock) events.push('activation-lock-enter')
        try {
          return await execute()
        } finally {
          if (options.trackActivationLock) events.push('activation-lock-exit')
        }
      },
      inspectNamespace: async () => namespace,
      removeUncommittedNamespace: async (candidate) => {
        const current = parent.namespace(candidate.entryName)
        if (current === undefined) return true
        if (!await current.isSameEntry(candidate.root)) return false
        await parent.removeEntry(candidate.entryName)
        return true
      },
      recoverCandidate: async ({ repository: target, candidate }) => {
        if (options.failure !== 'uncommitted-namespace-unknown') {
          return recoverWorkspaceActivationCandidate({
            repository: target,
            candidate,
            storage: {
              getDirectory: async () => parent as unknown as FileSystemDirectoryHandle,
            } as StorageManager & { getDirectory(): Promise<FileSystemDirectoryHandle> },
          })
        }
        const currentRecord = await target.readLifecycle(OPERATION_ID)
        const current = currentRecord === undefined
          ? initialReceiveLifecycleState({
              operationId: OPERATION_ID,
              receiveIntentDigest: planned.action.artifact.digest,
            })
          : decodeStoredReceiveLifecycleState(currentRecord)
        const lifecycle = nextReceiveLifecycleState(current, {
          kind: 'needs-attention',
          reason: 'target-ownership-unknown',
          lastVerifiedRecordDigest: candidate.digest,
        }) as Extract<ReturnType<typeof nextReceiveLifecycleState>, { kind: 'needs-attention' }>
        await target.commitTransition({
          operationId: OPERATION_ID,
          expectedLifecycleGeneration: current.generation,
          lifecycle,
        })
        return Object.freeze({ kind: 'needs-attention' as const, lifecycle })
      },
      acquireLease: async (targetRepository, operationId) => {
        const lease = receiveOperationLeaseRecord({
          operationId,
          leaseId: LEASE_ID,
          acquiredAt: 1_000,
        })
        await targetRepository.commitTransition({
          operationId,
          lease: { kind: 'put', record: lease },
        })
        events.push('lease-acquired')
        return Object.freeze({
          operationId,
          leaseId: LEASE_ID,
          acquiredAt: 1_000,
          heartbeat: async () => lease,
          release: async () => {
            await targetRepository.commitTransition({
              operationId,
              expectedLeaseId: LEASE_ID,
              lease: { kind: 'delete', leaseId: LEASE_ID },
            })
          },
        })
      },
      ...(options.failure === 'conservative-absence'
        ? {
            observePersistence: vi.fn()
              .mockRejectedValueOnce(new Error('persistence observation unavailable'))
              .mockResolvedValue(Object.freeze({ kind: 'absent' as const })),
          }
        : {}),
    },
    openRepository: async () => {
      events.push('repository-open')
      return repository
    },
    openNamespace: async (input) => {
      if (options.failure === 'before-namespace' || options.failure === 'conservative-absence') {
        throw new DOMException('namespace unavailable', 'NotFoundError')
      }
      events.push('namespace-create')
      if (options.failure === 'uncommitted-namespace-unknown') {
        const candidate = await journalWorkspaceActivation({
          repository,
          receiveIntent: input.receiveIntent,
          entryIdentity: activationCandidate.entryIdentity,
          workspaceRootHandleId: activationCandidate.rootHandleId,
          workspaceOwnedObjectId: activationCandidate.rootOwnedObjectId,
        })
        input.onActivationCandidateCommitted?.(candidate)
      }
      parent.installNamespace(namespace.entryName, root)
      if (options.failure === 'uncommitted-namespace-unknown') {
        parent.replaceNamespace()
        throw new OriginPrivateWorkspaceNamespaceOpenError({
          cause: new Error('namespace persistence failed'),
          candidate: activationCandidate,
          namespace,
        })
      }
      const candidate = await journalWorkspaceActivation({
        repository,
        receiveIntent: input.receiveIntent,
        entryIdentity: activationCandidate.entryIdentity,
        workspaceRootHandleId: namespace.rootHandleId,
        workspaceOwnedObjectId: namespace.rootOwnedObjectId,
      })
      input.onActivationCandidateCommitted?.(candidate)
      await promoteWorkspaceActivation({
        repository,
        candidate,
        workspaceRootHandle: namespace.root,
      })
      events.push('namespace-persisted')
      if (options.failure !== 'after-persistence-without-callback') {
        input.onPersistenceCommitted?.(namespace)
      }
      if (options.failure === 'abort-after-persistence') abort.abort('test abort')
      if (options.failure === 'after-persistence' ||
          options.failure === 'after-persistence-without-callback') {
        throw new Error('post-persistence verification failed')
      }
      return namespace
    },
    openStages: async (input) => {
      if (options.failure === 'after-lease') throw new Error('stages unavailable')
      events.push('stages-opened')
      return WorkspaceOperationStages.open(input)
    },
    createOperation: async (input) => {
      if (options.failure === 'during-runtime') throw new Error('runtime assembly failed')
      events.push('runtime-created')
      const operation = fakeBoundOperation(input.intent)
      boundOperations.push(operation)
      return operation
    },
    operationId: () => OPERATION_ID,
    workspaceId: () => WORKSPACE_ID,
    repositoryReference: () => REPOSITORY_REFERENCE,
  }
  const windowPort = Object.freeze({
    navigator: Object.freeze({
      storage: Object.freeze({ estimate: async () => ({ quota: 1, usage: 0 }) }),
      locks: Object.freeze({}),
    }),
  }) as unknown as BrowserReceiveWindow
  return {
    action: planned.action,
    selection: planned.selection,
    repository,
    parent,
    events,
    boundOperations,
    authority: () => new WorkspaceArtifactPresentationAuthority({
      windowPort,
      offered: planned.offered,
      dependencies,
    }),
    commit: async () => {
      const authority = new WorkspaceArtifactPresentationAuthority({
        windowPort,
        offered: planned.offered,
        dependencies,
      })
      return authority.commit({
        action: planned.action,
        signal: abort.signal,
        freezeAtFence: async (candidate) => {
          events.push('fence')
          return bindReceiveIntent({ selection: planned.selection, action: planned.action, candidate })
        },
      })
    },
  }
}

async function workspaceResolvedAction(quotaEstimate = 1_000_000n): Promise<{
  readonly selection: Awaited<ReturnType<typeof createSelectionSpec>>
  readonly offered: OfferedArtifactChoice
  readonly action: ResolvedArtifactAction
}> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const currentEnvironment = environment({
    targets: [handoffTarget('handoff-route')],
    workspace: { ...workspaceOffer('workspace-route'), quotaAvailabilityEstimateBytes: quotaEstimate },
  })
  const currentProjection = projection(selection, singleFileProof(), 1n)
  const offers = await offerArtifacts(currentProjection, COMPLETE_DISCOVERY, currentEnvironment)
  if (offers.kind !== 'artifact-actions') throw new Error('expected workspace action offers')
  const offered = [offers.primary, ...offers.alternatives].find((candidate) =>
    candidate.route.kind === 'workspace-then-publish')
  if (offered === undefined) throw new Error('expected workspace action')
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
  if (outcome.kind !== 'resolved') throw new Error('expected resolved workspace action')
  return Object.freeze({ selection, offered, action: outcome.action })
}

function requireOwned(result: V2RouteCommitResult): Extract<V2RouteCommitResult, { kind: 'owned-effects' }> {
  if (result.kind !== 'owned-effects') throw new Error('expected owned workspace effects')
  return result
}

function fakeBoundOperation(intent: ReceiveIntent): V2BoundReceiveOperation {
  const lifecycle = {
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation: 1n,
    kind: 'intent-frozen',
  } as const
  return Object.freeze({
    intent,
    plans: Object.freeze({}) as V2PlanExecutionAuthority,
    transferJobId: identity(90),
    lifecycle,
    activeControls: Object.freeze(['pause'] as const),
    interrupt: () => undefined,
    startLifecycleAction: async () => ({ lifecycle }),
    observeExpiry: async () => ({ lifecycle }),
    resolveWorkspaceUsage: () => null,
    settleTransferAdmissionFailure: async () => ({ lifecycle }),
    detach: () => undefined,
  })
}

class MemoryReceiveOperationRepository implements WorkspaceActivationJournalRepository {
  readonly #records = new Map<string, PersistedReceiveRecord>()
  readonly #pages = new Map<string, ManifestPageRecord>()
  readonly #handles = new Map<string, ReceiveOperationHandleRecord>()
  readonly #leases = new Map<string, ReceiveOperationLeaseRecord>()
  closed = false
  closeCount = 0
  #rejectNeedsAttention = 0
  #rejectClose = false

  close(): void {
    this.closeCount += 1
    if (this.#rejectClose) {
      this.#rejectClose = false
      throw new Error('repository close failed')
    }
    this.closed = true
  }

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    if (this.#rejectNeedsAttention > 0 && transition.lifecycle?.kind === 'needs-attention') {
      this.#rejectNeedsAttention -= 1
      throw new DOMException('NeedsAttention write failed', 'QuotaExceededError')
    }
    const prepared = await prepareReceiveOperationTransition(transition)
    for (const id of prepared.deleteRecordIds) this.#records.delete(id)
    for (const id of prepared.deleteManifestPageIds) this.#pages.delete(id)
    for (const id of prepared.deleteHandleIds) this.#handles.delete(id)
    for (const record of prepared.records) this.#records.set(record.id, record)
    for (const page of prepared.manifestPages) this.#pages.set(page.id, page)
    for (const handle of prepared.handles) this.#handles.set(handle.id, handle)
    if (prepared.lease?.kind === 'put') {
      this.#leases.set(prepared.operationId, prepared.lease.record)
    } else if (prepared.lease?.kind === 'delete') {
      this.#leases.delete(prepared.operationId)
    }
  }

  rejectNextNeedsAttentionTransitions(count: number): void { this.#rejectNeedsAttention = count }
  rejectNextClose(): void { this.#rejectClose = true }

  readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    return Promise.resolve(this.#records.get(id))
  }

  readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined> {
    return Promise.resolve(this.records(operationId).find((record) =>
      record.kind === RECEIVE_RECORD_LIFECYCLE_STATE))
  }

  listRecords(operationId: string, kind?: ReceiveRecordKind): Promise<readonly PersistedReceiveRecord[]> {
    return Promise.resolve(this.records(operationId).filter((record) =>
      kind === undefined || record.kind === kind))
  }

  listManifestPages(operationId: string, kind?: ReceiveRecordKind): Promise<readonly ManifestPageRecord[]> {
    return Promise.resolve([...this.#pages.values()].filter((page) =>
      page.operationId === operationId && (kind === undefined || page.kind === kind)))
  }

  readHandle<T = unknown>(id: string): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    return Promise.resolve(this.#handles.get(id) as ReceiveOperationHandleRecord<T> | undefined)
  }

  listHandles(operationId: string): Promise<readonly ReceiveOperationHandleRecord[]> {
    return Promise.resolve([...this.#handles.values()].filter(handle =>
      handle.operationId === operationId))
  }

  async listWorkspaceActivationCandidates() {
    return Promise.all([...this.#records.values()]
      .filter(record => record.kind === RECEIVE_RECORD_WORKSPACE_ACTIVATION)
      .map(decodeStoredWorkspaceActivationCandidate))
  }

  listInitialWorkspaceActivationOperationIds(): Promise<readonly string[]> {
    return Promise.resolve([...this.#records.values()]
      .filter(record => record.kind === RECEIVE_RECORD_LIFECYCLE_STATE)
      .map(decodeStoredReceiveLifecycleState)
      .filter(lifecycle => lifecycle.kind === 'intent-frozen')
      .map(lifecycle => lifecycle.operationId))
  }

  readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined> {
    return Promise.resolve(this.#leases.get(operationId))
  }

  records(operationId: string): readonly PersistedReceiveRecord[] {
    return [...this.#records.values()].filter((record) => record.operationId === operationId)
  }
}

class MemoryDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #token = crypto.randomUUID()
  readonly #entries = new Map<string, MemoryDirectoryHandle>()
  #marker: string | undefined

  constructor(name: string) {
    this.name = name
  }

  get hasNamespace(): boolean { return this.#entries.size !== 0 }

  installNamespace(name: string, root: MemoryDirectoryHandle): void {
    this.#entries.set(name, root)
  }

  installMarker(value: string): void { this.#marker = value }

  replaceNamespace(): void {
    const name = this.#entries.keys().next().value as string | undefined
    if (name !== undefined) this.#entries.set(name, new MemoryDirectoryHandle('replacement'))
  }

  namespace(name: string): MemoryDirectoryHandle | undefined {
    return this.#entries.get(name)
  }

  async getDirectoryHandle(name: string): Promise<FileSystemDirectoryHandle> {
    const entry = this.#entries.get(name)
    if (entry === undefined) throw new DOMException('missing', 'NotFoundError')
    return entry as unknown as FileSystemDirectoryHandle
  }

  async removeEntry(name: string): Promise<void> {
    if (!this.#entries.delete(name)) throw new DOMException('missing', 'NotFoundError')
  }

  getFileHandle(): Promise<FileSystemFileHandle> {
    if (this.#marker === undefined) {
      return Promise.reject(new DOMException('missing', 'NotFoundError'))
    }
    return Promise.resolve({
      kind: 'file',
      name: '.windshare-workspace-owner-v1',
      getFile: async () => ({ text: async () => this.#marker }) as File,
    } as unknown as FileSystemFileHandle)
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return (other as unknown as MemoryDirectoryHandle).#token === this.#token
  }
}
