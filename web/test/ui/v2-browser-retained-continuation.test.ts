import { describe, expect, it, vi } from 'vitest'

import { createIncidentScopeIssuer } from '../../src/diagnostics/incident'
import { createAttemptOutputFailureCapability } from '../../src/output/diagnostics'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
} from '../../src/output/resume/reopen-authority'
import { createBrowserReceiveComposition } from '../../src/ui/v2-browser-receive-composition'
import {
  FakeResumeSource,
  capableWindow,
  directoryHandle,
  fakeContinuation,
  fakeContinuationExecutor,
  fakeContinuationRuntime,
  identity,
  mutationPort,
  receiveLifecycle,
  retainedRecoverySummary,
} from './v2-browser-receive-composition-fixture'

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
      payloadKind: 'file-set',
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
      const scope = createIncidentScopeIssuer().open('retained_action')
      const failures = createAttemptOutputFailureCapability(scope.handle)

      await expect(inventory.act(
        operation,
        'continue',
        new AbortController().signal,
        failures.sinks,
      ))
        .resolves.toEqual({ kind: 'receive-continuation', runtime })

      expect(resumeReceive).toHaveBeenCalledWith(
        continuation,
        expect.any(AbortSignal),
        failures.sinks,
      )
      expect(failures.sinks.attempt?.claim()?.scope).toEqual(scope.identity)
      expect(close).not.toHaveBeenCalled()
      await runtime.detach()
      expect(close).toHaveBeenCalledTimes(1)
      failures.revoke()
      scope.close()
      inventory.close()
    },
  )

  it.each([
    ['continue', 'preserve'],
    ['redownload', 'restart-owned-file'],
  ] as const)('carries retained DirectTree %s authority through reopen as %s', async (
    action,
    retainedFileRecovery,
  ) => {
    const lifecycle = receiveLifecycle(60, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(61, 32),
      completedFileCount: 1n,
      completedBytes: 64n,
      expiresAt: 5_000,
    })
    const summary = retainedRecoverySummary(lifecycle)
    const source = new FakeResumeSource([lifecycle], summary)
    const close = vi.fn(async () => undefined)
    const continuation = fakeContinuation('direct-tree-receive', close)
    const runtime = fakeContinuationRuntime(close)
    const resume = vi.fn(async () => Object.freeze({
      kind: 'continuation' as const,
      continuation,
    }))
    const resumeReceive = vi.fn(async () => runtime)
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutationPort(resume),
        continuationExecutor: fakeContinuationExecutor({ resumeReceive }),
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]!

    expect(operation.recoverySummary).toBe(summary)
    expect(operation.actions).toEqual(['continue', 'redownload'])
    await expect(inventory.act(operation, action, new AbortController().signal))
      .resolves.toEqual({ kind: 'receive-continuation', runtime })
    expect(resume).toHaveBeenCalledWith(
      expect.any(Object),
      { retainedFileRecovery },
    )
    expect(resumeReceive).toHaveBeenCalledWith(
      continuation,
      expect.any(AbortSignal),
    )
    await runtime.detach()
    inventory.close()
  })

  it('records retained workspace ownership attention without restoring the continuation', async () => {
    const fallback = receiveLifecycle(41, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
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

  it('preserves the continuation trigger before its cleanup consequence', async () => {
    const source = new FakeResumeSource([receiveLifecycle(48, {
      kind: 'resumable-package',
      sealedMaterializationDigest: identity(49, 32),
      tempCleanupProofDigest: identity(50, 32),
      expiresAt: 5_000,
    })])
    const trigger = new DOMException('package continuation failed', 'OperationError')
    const consequence = new DOMException('checkpoint close failed', 'InvalidStateError')
    const continuation = fakeContinuation(
      'workspace-package',
      vi.fn(() => Promise.reject(consequence)),
    )
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutationPort(vi.fn(async () => Object.freeze({
          kind: 'continuation' as const,
          continuation,
        }))),
        continuationExecutor: fakeContinuationExecutor({
          resumePackage: () => Promise.reject(trigger),
        }),
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('package continuation was not projected')

    const result = inventory.act(operation, 'continue', new AbortController().signal)
    const rejected = await Promise.resolve(result).then(
      () => undefined,
      (error: unknown) => error,
    )
    expect(rejected).toBeInstanceOf(AggregateError)
    const aggregate = rejected as AggregateError
    expect(aggregate.errors).toEqual([trigger, consequence])
    expect(aggregate.cause).toBe(trigger)
    inventory.close()
  })

})
