import { describe, expect, it, vi } from 'vitest'

import {
  bindMaterialization,
  offerArtifacts,
} from '../../src/output/planning'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import type {
  ReceiveOperationMutationPort,
  ReceiveOperationResumeSource,
} from '../../src/output/resume/authority'
import type { ReceiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
} from '../../src/output/resume/reopen-authority'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type {
  ReceiveLifecycleState,
  ReceiveLifecycleStatePayload,
} from '../../src/output/workspace'
import { createSelectionSpec } from '../../src/transfer/intent'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
  type BrowserRetainedContinuationExecutor,
} from '../../src/ui/v2-browser-receive-composition'
import type { V2BoundReceiveOperation } from '../../src/ui/v2-receive-runtime'
import {
  COMPLETE_DISCOVERY,
  projection,
  treeProof,
} from '../output/planning/fixture'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity as binaryIdentity,
  identityText as binaryIdentityText,
  readerFixture,
  transferJobFixture,
} from '../transfer/v2-job-fixture'

describe('browser production receive composition', () => {
  it('advertises only complete installed route assemblies and never probes with a picker', async () => {
    const picker = vi.fn(async () => directoryHandle())
    const windowPort = capableWindow(picker)
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(picker).not.toHaveBeenCalled()
    expect(environment.targets.map(target => target.kind)).toEqual([
      'fsa-parent-directory',
      'browser-handoff',
    ])
    expect(environment.targets.some(target => target.kind === 'managed-atomic-file-target')).toBe(false)
    expect(environment.workspace?.kind).toBe('origin-private-workspace')
    expect(environment.portable?.kind).toBe('portable-memory')
  })

  it('removes FSA, workspace, and portable offers when their production registry owner is absent', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    Object.defineProperty(windowPort, 'indexedDB', { value: undefined })
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(environment.targets.map(target => target.kind)).toEqual(['browser-handoff'])
    expect(environment.workspace).toBeNull()
    expect(environment.portable).toBeNull()
  })

  it('projects every retained v6 continuation without starting picker or handoff authority', async () => {
    const picker = vi.fn(async () => directoryHandle())
    const windowPort = capableWindow(picker)
    const source = new FakeResumeSource(retainedLifecycles())
    const composition = createBrowserReceiveComposition(windowPort, {
      openResumeSource: async () => source,
      now: () => 1_000,
    })

    const inventory = await composition.retained.list(new AbortController().signal)
    const { operations } = inventory
    const continuations = Object.fromEntries(operations.map((operation) => [
      operation.lifecycle.kind,
      operation.continuation,
    ]))

    expect(continuations).toEqual({
      'download-started': 'retry-download',
      expired: 'cleanup-expired',
      'needs-attention': 'needs-attention',
      published: 'retry-cleanup',
      'resumable-package': 'resume-package',
      'resumable-receive': 'resume-receive',
      'waiting-to-save': 'save-artifact',
    })
    expect(operations).toHaveLength(7)
    expect(operations[0]?.lifecycle).not.toHaveProperty('verifiedRanges')
    expect(operations.every(operation => operation.actions.length === 0)).toBe(true)
    expect(source.closeCalls).toBe(0)
    inventory.close()
    expect(source.closeCalls).toBe(1)
    expect(picker).not.toHaveBeenCalled()
    expect(windowPort.URL.createObjectURL).not.toHaveBeenCalled()
  })

  it('turns an elapsed stable deadline into cleanup-only inventory', async () => {
    const source = new FakeResumeSource([receiveLifecycle(20, {
      kind: 'resumable-receive',
      checkpointSetDigest: identity(21, 32),
      completedFileCount: 3n,
      completedBytes: 512n,
      expiresAt: 1_000,
    })])
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      { openResumeSource: async () => source, now: () => 1_000 },
    )

    const inventory = await composition.retained.list(new AbortController().signal)

    expect(inventory.operations).toMatchObject([{
      continuation: 'cleanup-expired',
      expiresAt: 1_000,
      lifecycleGeneration: 1n,
      actions: [],
    }])
    expect(source.closeCalls).toBe(0)
    inventory.close()
    expect(source.closeCalls).toBe(1)
  })

  it('keeps the live resume reference until expiry cleanup consumes the exact inventory token', async () => {
    const source = new FakeResumeSource([receiveLifecycle(22, {
      kind: 'resumable-receive',
      checkpointSetDigest: identity(23, 32),
      completedFileCount: 1n,
      completedBytes: 64n,
      expiresAt: 1_000,
    })])
    const expiredDescriptors: ReceiveOperationResumeDescriptor[] = []
    const expire = vi.fn(async (descriptor: ReceiveOperationResumeDescriptor) => {
      expiredDescriptors.push(descriptor)
      return Object.freeze({
        kind: 'retention-cleanup',
        result: Object.freeze({ kind: 'already-absent' }),
      }) as AuthorityOwnedReceiveOperationMutationResult
    })
    const discard = vi.fn(async () => Object.freeze({ kind: 'already-absent' } as const))
    const mutations: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> =
      Object.freeze({
        resume: () => Promise.reject(new Error('unexpected resume')),
        expire,
        discard,
      })
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutations,
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('expired operation was not projected')

    expect(operation.actions).toEqual(['delete'])
    await expect(inventory.act(
      Object.freeze({ ...operation }),
      'delete',
      new AbortController().signal,
    )).rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(expire).not.toHaveBeenCalled()

    await inventory.act(operation, 'delete', new AbortController().signal)

    expect(expire).toHaveBeenCalledTimes(1)
    expect(expiredDescriptors[0]?.operationId).toBe(operation.operationId)
    expect(discard).not.toHaveBeenCalled()
    expect(source.closeCalls).toBe(0)
    inventory.close()
    expect(source.closeCalls).toBe(1)
  })

  it('enables Continue and authority-owned discard only when mutation authority is installed', async () => {
    const source = new FakeResumeSource([receiveLifecycle(26, {
      kind: 'resumable-receive',
      checkpointSetDigest: identity(27, 32),
      completedFileCount: 1n,
      completedBytes: 64n,
      expiresAt: 5_000,
    })])
    const discardedDescriptors: ReceiveOperationResumeDescriptor[] = []
    const discard = vi.fn(async (descriptor: ReceiveOperationResumeDescriptor) => {
      discardedDescriptors.push(descriptor)
      return Object.freeze({ kind: 'already-absent' } as const)
    })
    const mutations: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> =
      Object.freeze({
        resume: () => Promise.reject(new Error('unexpected resume')),
        expire: () => Promise.reject(new Error('unexpected expiry')),
        discard,
      })
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutations,
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('resumable operation was not projected')

    expect(operation.actions).toEqual(['continue', 'discard'])
    expect(operation.unavailableReason).toBeUndefined()
    await inventory.act(operation, 'discard', new AbortController().signal)

    expect(discardedDescriptors[0]?.lifecycle).toBe(operation.lifecycle)
    inventory.close()
  })

})

describe('browser retained continuation composition', () => {
  it('passes a waiting package from the live reference to the retained continuation owner', async () => {
    const source = new FakeResumeSource([receiveLifecycle(24, {
      kind: 'waiting-to-save',
      packageDigest: identity(25, 32),
      expiresAt: 5_000,
    })])
    const close = vi.fn(async () => undefined)
    const continuation = fakeContinuation('workspace-retained', close)
    const resumeResult: AuthorityOwnedReceiveOperationMutationResult = Object.freeze({
      kind: 'continuation',
      continuation,
    })
    const resume = vi.fn(async () => resumeResult)
    const mutations = mutationPort(resume)
    const continueRetained = vi.fn(async () => undefined)
    const continuationExecutor = fakeContinuationExecutor({ continueRetained })
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutations,
        continuationExecutor,
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('waiting operation was not projected')

    expect(operation.actions).toEqual(['save', 'discard'])
    await expect(inventory.act(operation, 'save', new AbortController().signal))
      .resolves.toEqual({ kind: 'completed' })

    expect(resume).toHaveBeenCalledTimes(1)
    expect(continueRetained).toHaveBeenCalledWith(
      continuation,
      'save',
      expect.any(AbortSignal),
    )
    expect(close).toHaveBeenCalledTimes(1)
    inventory.close()
  })

  it.each(['direct-tree-receive', 'workspace-receive'] as const)(
    'transfers %s ownership to the resumed runtime until detach',
    async (kind) => {
      const source = new FakeResumeSource([receiveLifecycle(40, {
        kind: 'resumable-receive',
        checkpointSetDigest: identity(41, 32),
        completedFileCount: 1n,
        completedBytes: 64n,
        expiresAt: 5_000,
      })])
      const close = vi.fn(async () => undefined)
      const continuation = fakeContinuation(kind, close)
      const runtime = fakeContinuationRuntime(close)
      const resumeReceive = vi.fn(async () => runtime)
      const composition = createBrowserReceiveComposition(
        capableWindow(vi.fn(async () => directoryHandle())),
        {
          openResumeSource: async () => source,
          resumeMutations: mutationPort(vi.fn(async () => Object.freeze({
            kind: 'continuation' as const,
            continuation,
          }))),
          continuationExecutor: fakeContinuationExecutor({ resumeReceive }),
          now: () => 1_000,
        },
      )
      const inventory = await composition.retained.list(new AbortController().signal)
      const operation = inventory.operations[0]
      if (operation === undefined) throw new Error('receive continuation was not projected')

      await expect(inventory.act(operation, 'continue', new AbortController().signal))
        .resolves.toEqual({ kind: 'receive-continuation', runtime })

      expect(resumeReceive).toHaveBeenCalledWith(continuation, expect.any(AbortSignal))
      expect(close).not.toHaveBeenCalled()
      await runtime.detach()
      expect(close).toHaveBeenCalledTimes(1)
      inventory.close()
    },
  )

  it('records retained workspace ownership attention without restoring the continuation', async () => {
    const fallback = receiveLifecycle(41, {
      kind: 'resumable-receive',
      checkpointSetDigest: identity(42, 32),
      completedFileCount: 1n,
      completedBytes: 64n,
      expiresAt: 5_000,
    })
    if (fallback.kind !== 'resumable-receive') throw new Error('test fallback changed kind')
    const failure = new TargetOwnershipUnknownError('checkpoint', fallback.operationId)
    const close = vi.fn(async () => undefined)
    const restoreReceiveContinuation = vi.fn(async () => fallback)
    const attention = Object.freeze({
      kind: 'needs-attention' as const,
      operationId: fallback.operationId,
      receiveIntentDigest: fallback.receiveIntentDigest,
      generation: fallback.generation + 2n,
      reason: 'target-ownership-unknown' as const,
      lastVerifiedRecordDigest: identity(43, 32),
    })
    const recordTargetOwnershipUnknown = vi.fn(async () => attention)
    const traceNames: string[] = []
    const continuation = Object.freeze({
      kind: 'workspace-receive' as const,
      operation: Object.freeze({
        kind: 'workspace' as const,
        intent: Object.freeze({
          operationId: fallback.operationId,
          digest: fallback.receiveIntentDigest,
        }),
        lifecycle: Object.freeze({
          ...fallback,
          kind: 'receiving' as const,
          generation: fallback.generation + 1n,
          activeLeaseId: identity(44),
        }),
        receiveAdmissionFallback: fallback,
        admittedContent: Object.freeze({}),
        receiveContinuation: Object.freeze({
          openBackend: async (options?: {
            readonly onTrace?: (event: {
              readonly name: 'receive.operation.needs_attention'
              readonly operation_id: string
              readonly prior_state: 'receiving'
              readonly needs_attention_reason: 'target-ownership-unknown'
            }) => void
          }) => {
            options?.onTrace?.(Object.freeze({
              name: 'receive.operation.needs_attention',
              operation_id: fallback.operationId,
              prior_state: 'receiving',
              needs_attention_reason: 'target-ownership-unknown',
            }))
            throw failure
          },
        }),
        stages: Object.freeze({
          restoreReceiveContinuation,
          recordTargetOwnershipUnknown,
        }),
        close,
      }),
    }) as unknown as AuthorityOwnedReceiveOperationContinuation
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => new FakeResumeSource([fallback]),
        resumeMutations: mutationPort(vi.fn(async () => Object.freeze({
          kind: 'continuation' as const,
          continuation,
        }))),
        now: () => 1_000,
        onTrace: event => traceNames.push(event.name),
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('resumable operation was not projected')

    await expect(inventory.act(operation, 'continue', new AbortController().signal))
      .rejects.toBe(failure)

    expect(recordTargetOwnershipUnknown).toHaveBeenCalledWith(fallback.receiveIntentDigest)
    expect(restoreReceiveContinuation).not.toHaveBeenCalled()
    expect(traceNames).toContain('receive.operation.needs_attention')
    expect(close).toHaveBeenCalledOnce()
    inventory.close()
  })

  it('executes only the output-owned package continuation and closes its authority', async () => {
    const source = new FakeResumeSource([receiveLifecycle(42, {
      kind: 'resumable-package',
      sealedMaterializationDigest: identity(43, 32),
      tempCleanupProofDigest: identity(44, 32),
      expiresAt: 5_000,
    })])
    const close = vi.fn(async () => undefined)
    const continuation = fakeContinuation('workspace-package', close)
    const resumePackage = vi.fn(async () => undefined)
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutationPort(vi.fn(async () => Object.freeze({
          kind: 'continuation' as const,
          continuation,
        }))),
        continuationExecutor: fakeContinuationExecutor({ resumePackage }),
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('package continuation was not projected')

    await expect(inventory.act(operation, 'continue', new AbortController().signal))
      .resolves.toEqual({ kind: 'completed' })

    expect(resumePackage).toHaveBeenCalledWith(continuation, expect.any(AbortSignal))
    expect(close).toHaveBeenCalledTimes(1)
    inventory.close()
  })

  it('closes package authority when continuation execution fails', async () => {
    const source = new FakeResumeSource([receiveLifecycle(45, {
      kind: 'resumable-package',
      sealedMaterializationDigest: identity(46, 32),
      tempCleanupProofDigest: identity(47, 32),
      expiresAt: 5_000,
    })])
    const close = vi.fn(async () => undefined)
    const continuation = fakeContinuation('workspace-package', close)
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutationPort(vi.fn(async () => Object.freeze({
          kind: 'continuation' as const,
          continuation,
        }))),
        continuationExecutor: fakeContinuationExecutor({
          resumePackage: () => Promise.reject(new DOMException('unknown output state', 'OperationError')),
        }),
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('package continuation was not projected')

    await expect(inventory.act(operation, 'continue', new AbortController().signal))
      .rejects.toMatchObject({ name: 'OperationError' })
    expect(close).toHaveBeenCalledTimes(1)
    inventory.close()
  })

  it('starts exactly one FSA picker synchronously in the explicit action stack', async () => {
    let inActionStack = true
    const calls: boolean[] = []
    const picker = vi.fn(async () => {
      calls.push(inActionStack)
      return directoryHandle()
    })
    const windowPort = capableWindow(picker)
    const composition = createBrowserReceiveComposition(windowPort)
    const selection = await testSelection()
    const environment = await composition.environment(new AbortController().signal)
    const offered = await offerArtifacts(
      projection(selection, treeProof()),
      COMPLETE_DISCOVERY,
      environment,
    )
    if (offered.kind !== 'artifact-actions') throw new Error('tree action was not offered')
    const action = [offered.primary, ...offered.alternatives]
      .find(candidate => candidate.plan.kind === 'direct-tree')
    if (action === undefined) throw new Error('FSA DirectTree action was not offered')

    const authority = composition.startArtifactAuthority(action)
    inActionStack = false

    expect(picker).toHaveBeenCalledTimes(1)
    expect(calls).toEqual([true])
    await Promise.resolve(authority).then(value => value.release('test completed'))
  })

  it('runs a timestamped portable artifact through the real operation-bound plan authority', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    Object.defineProperty(windowPort.navigator, 'locks', { value: undefined })
    const composition = createBrowserReceiveComposition(windowPort)
    const selection = await createSelectionSpec({
      shareInstance: binaryIdentityText(1),
      syntheticRoot: binaryIdentityText(2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })
    const environment = await composition.environment(new AbortController().signal)
    const state = projection(selection, {
      kind: 'single-file',
      file: {
        fileId: binaryIdentityText(20),
        sourcePath: 'docs/report.txt',
        portableName: 'report.txt',
      },
    }, 3n)
    const offered = await offerArtifacts(state, COMPLETE_DISCOVERY, environment)
    if (offered.kind !== 'artifact-actions') throw new Error('file action was not offered')
    const action = [offered.primary, ...offered.alternatives]
      .find(candidate => candidate.plan.kind === 'portable-handoff')
    if (action === undefined) throw new Error('portable action was not offered')
    const started = await composition.startArtifactAuthority(action)

    const runtime = await started.finalize(async acquired => {
      const bound = await bindMaterialization({
        selection,
        chosenAction: action,
        currentProjection: state,
        currentDiscovery: COMPLETE_DISCOVERY,
        currentEnvironment: environment,
        acquired,
      })
      if (bound.kind !== 'bound') throw new Error('portable action did not freeze')
      return bound.intent
    }, new AbortController().signal)

    expect(runtime.intent.plan.kind).toBe('portable-handoff')
    expect(runtime.transferJobId).toHaveLength(22)
    expect(runtime.plans.preparePortable).toBeTypeOf('function')
    expect(runtime.lifecycle.kind).toBe('intent-frozen')

    const file = Object.freeze({
      ...fileEntry(binaryIdentity(20), 'report.txt', 3n),
      modifiedTime: Object.freeze({
        seconds: 1_700_000_000n,
        nanoseconds: 123_000_000,
        precision: 3 as const,
        milliseconds: 1_700_000_000_123n,
      }),
    })
    const docs = directoryEntry(binaryIdentity(30), 'docs')
    const { catalog } = catalogFixture([
      { id: binaryIdentity(2), entries: [docs] },
      { id: docs.id, entries: [file] },
    ])
    const readers = readerFixture([file])
    const traceNames: string[] = []
    const createObjectURL = vi.mocked(windowPort.URL.createObjectURL)
    const objectURLCallsBeforeAttempt = createObjectURL.mock.calls.length
    const result = await transferJobFixture({
      catalog,
      selection: new V2SelectionPolicy(true),
      intent: runtime.intent,
      plans: runtime.plans,
      revisions: readers.revisions,
      broker: readers.broker,
      onTrace: event => traceNames.push(event.name),
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe('download-started')
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).not.toHaveLength(0)
    expect(traceNames).toContain('receive.materialization.completed')
    expect(traceNames).not.toContain('receive.materialization.failed')
    expect(createObjectURL.mock.calls).toHaveLength(objectURLCallsBeforeAttempt + 1)
    const [publishedArtifact] = createObjectURL.mock.calls.at(-1) ?? []
    if (!(publishedArtifact instanceof Blob)) {
      throw new TypeError('portable attempt did not publish a Blob-backed artifact')
    }
    expect(publishedArtifact.size).toBe(3)
    await runtime.detach()
  })
})

type ContinuationKind = AuthorityOwnedReceiveOperationContinuation['kind']

function fakeContinuation<Kind extends ContinuationKind>(
  kind: Kind,
  close: () => Promise<void>,
): Extract<AuthorityOwnedReceiveOperationContinuation, { readonly kind: Kind }> {
  return Object.freeze({
    kind,
    operation: Object.freeze({ close }),
  }) as unknown as Extract<AuthorityOwnedReceiveOperationContinuation, { readonly kind: Kind }>
}

function mutationPort(
  resume: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult>['resume'],
): ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> {
  return Object.freeze({
    resume,
    expire: () => Promise.reject(new Error('unexpected expiry')),
    discard: () => Promise.resolve(Object.freeze({ kind: 'already-absent' })),
  })
}

function fakeContinuationExecutor(
  overrides: Partial<BrowserRetainedContinuationExecutor>,
): BrowserRetainedContinuationExecutor {
  return Object.freeze({
    resumeReceive: () => Promise.reject(new Error('unexpected receive continuation')),
    resumePackage: () => Promise.reject(new Error('unexpected package continuation')),
    continueRetained: () => Promise.reject(new Error('unexpected retained continuation')),
    ...overrides,
  })
}

function fakeContinuationRuntime(
  close: () => Promise<void>,
): V2BoundReceiveOperation {
  let detached = false
  return Object.freeze({
    detach: async () => {
      if (detached) return
      detached = true
      await close()
    },
  }) as unknown as V2BoundReceiveOperation
}

class FakeResumeSource implements ReceiveOperationResumeSource {
  readonly #lifecycles: readonly ReceiveLifecycleState[]
  closeCalls = 0

  constructor(lifecycles: readonly ReceiveLifecycleState[]) {
    this.#lifecycles = Object.freeze([...lifecycles])
  }

  listLifecycleStates(): Promise<readonly ReceiveLifecycleState[]> {
    return Promise.resolve(this.#lifecycles)
  }

  close(): void {
    this.closeCalls += 1
  }
}

function retainedLifecycles(): readonly ReceiveLifecycleState[] {
  return Object.freeze([
    receiveLifecycle(1, {
      kind: 'resumable-receive',
      checkpointSetDigest: identity(30, 32),
      completedFileCount: 2n,
      completedBytes: 256n,
      expiresAt: 5_000,
    }),
    receiveLifecycle(2, {
      kind: 'resumable-package',
      sealedMaterializationDigest: identity(31, 32),
      tempCleanupProofDigest: identity(32, 32),
      expiresAt: 5_000,
    }),
    receiveLifecycle(3, {
      kind: 'waiting-to-save',
      packageDigest: identity(33, 32),
      expiresAt: 5_000,
    }),
    receiveLifecycle(4, {
      kind: 'download-started',
      attemptKind: 'workspace',
      attemptId: identity(34),
      packageDigest: identity(35, 32),
      retryableUntil: 5_000,
    }),
    receiveLifecycle(5, {
      kind: 'expired',
      priorStableState: 'waiting-to-save',
      expiresAt: 900,
      cleanupState: 'cleanup-pending',
      expiryReceiptDigest: identity(36, 32),
    }),
    receiveLifecycle(6, {
      kind: 'published',
      receiptDigest: identity(37, 32),
      cleanupState: 'cleanup-pending',
    }),
    receiveLifecycle(7, {
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
      lastVerifiedRecordDigest: identity(38, 32),
    }),
    receiveLifecycle(8, {
      kind: 'published',
      receiptDigest: identity(39, 32),
      cleanupState: 'clean',
    }),
  ])
}

function receiveLifecycle(
  seed: number,
  payload: ReceiveLifecycleStatePayload,
): ReceiveLifecycleState {
  return Object.freeze({
    ...payload,
    operationId: identity(seed),
    receiveIntentDigest: identity(seed + 40, 32),
    generation: 1n,
  }) as ReceiveLifecycleState
}

async function testSelection() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

function capableWindow(
  showDirectoryPicker: NonNullable<BrowserReceiveWindow['showDirectoryPicker']>,
): BrowserReceiveWindow {
  class TestFile extends Blob {
    readonly name: string
    readonly lastModified = 0
    readonly webkitRelativePath = ''

    constructor(parts: BlobPart[], name: string) {
      super(parts)
      this.name = name
    }
  }
  const anchor = {
    download: '',
    href: '',
    hidden: false,
    click: vi.fn(),
    remove: vi.fn(),
  }
  const candidate = {
    indexedDB: { open: vi.fn() },
    navigator: {
      locks: { request: vi.fn() },
      storage: {
        getDirectory: vi.fn(),
        estimate: vi.fn(async () => ({ usage: 1_024, quota: 8_192 })),
      },
    },
    showDirectoryPicker,
    URL: {
      createObjectURL: vi.fn(() => 'blob:windshare-test'),
      revokeObjectURL: vi.fn(),
    },
    Blob,
    File: TestFile,
    WritableStream,
    document: {
      createElement: vi.fn(() => anchor),
      documentElement: { append: vi.fn() },
    },
    setTimeout,
    clearTimeout,
  }
  return candidate as unknown as BrowserReceiveWindow
}

function directoryHandle(): FileSystemDirectoryHandle {
  return Object.freeze({ kind: 'directory', name: 'downloads' }) as FileSystemDirectoryHandle
}

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return Buffer.from(bytes).toString('base64url')
}
