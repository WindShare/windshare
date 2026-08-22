import { describe, expect, it, vi } from 'vitest'
import type { ReceiveIntent } from '../../../../src/transfer/intent'
import type { ReceiveLifecycleState } from '../../../../src/output/workspace/state'
import {
  deleteRetainedDirectZipSession,
  reopenDirectZipSession,
  type DirectZipRecoveryLifecyclePort,
  type DirectZipRecoverySessionInput,
} from '../../../../src/output/direct-zip/session'
import type {
  DirectZipLifecycleDecision,
  DirectZipTargetPort,
} from '../../../../src/output/direct-zip/target'

type Parent = Readonly<{ id: string }>
type FileHandle = Readonly<{ id: string }>
type Runtime = Readonly<{ id: string }>

describe('Direct ZIP retained session', () => {
  it('persists authorization gating without opening execution', async () => {
    const fixture = recoveryFixture(gated({
      kind: 'authorization-required', stage: 'permission-request', reason: 'permission-prompt',
    }))
    const result = await reopenDirectZipSession(fixture.input)
    expect(result.kind).toBe('gated')
    expect(fixture.lifecycle.gate).toHaveBeenCalledWith(expect.objectContaining({
      kind: 'authorization-required',
    }), {})
    expect(fixture.lifecycle.activate).not.toHaveBeenCalled()
  })

  it('runs slow verification only through the explicit verifier', async () => {
    const verification: DirectZipLifecycleDecision = {
      kind: 'target-verification-required',
      stage: 'snapshot',
      reason: 'observation-changed',
      proof: 'fresh-observation',
    }
    const target = targetSequence([gated(verification), readyReopen()])
    const fixture = recoveryFixture(undefined, target)
    const first = await reopenDirectZipSession(fixture.input)
    expect(first.kind).toBe('gated')
    if (first.kind !== 'gated') return
    expect(target.reopen).toHaveBeenCalledTimes(1)
    const verified = await first.verify!()
    expect(target.reopen).toHaveBeenLastCalledWith(expect.objectContaining({
      verifyChangedEvidence: true,
    }))
    expect(verified.kind).toBe('active')
  })

  it('persists destination-space-required after target resolution', async () => {
    const fixture = recoveryFixture(readyReopen(), undefined, 1n)
    const result = await reopenDirectZipSession(fixture.input)
    expect(result.kind).toBe('gated')
    if (result.kind !== 'gated') return
    expect(result.decision.kind).toBe('destination-space-required')
    expect(fixture.lifecycle.activate).not.toHaveBeenCalled()
  })

  it('keeps records when ownership-safe deletion is refused', async () => {
    const cleanupDecision: DirectZipLifecycleDecision = {
      kind: 'needs-attention', stage: 'cleanup-delete', reason: 'cleanup-refused',
    }
    const target = targetSequence([readyReopen()], gated(cleanupDecision))
    const fixture = recoveryFixture(undefined, target)
    const reopened = await reopenDirectZipSession(fixture.input)
    expect(reopened.kind).toBe('active')
    if (reopened.kind !== 'active') return
    const result = await reopened.session.delete(true)
    expect(result.kind).toBe('gated')
    expect(fixture.lifecycle.deleteOwnedTarget).not.toHaveBeenCalled()
  })

  it('deletes durable ownership only after exact target deletion verifies', async () => {
    const target = targetSequence([readyReopen()], {
      kind: 'ready', value: { disposition: 'deleted' },
    })
    const fixture = recoveryFixture(undefined, target)
    const reopened = await reopenDirectZipSession(fixture.input)
    if (reopened.kind !== 'active') throw new Error('expected active session')
    await expect(reopened.session.delete(true)).resolves.toEqual({ kind: 'deleted' })
    expect(fixture.lifecycle.deleteOwnedTarget).toHaveBeenCalledOnce()
  })

  it('cleans an expired retained target without activating transfer execution', async () => {
    const target = targetSequence([], { kind: 'ready', value: { disposition: 'deleted' } })
    const fixture = recoveryFixture(undefined, target)
    const input = { ...fixture.input, candidate: { id: 'candidate-proof' } as never }

    await expect(deleteRetainedDirectZipSession(
      input,
      true,
      new AbortController().signal,
    )).resolves.toEqual({ kind: 'deleted' })

    expect(fixture.lifecycle.prepareRetainedCleanup).toHaveBeenCalledOnce()
    expect(fixture.lifecycle.activate).not.toHaveBeenCalled()
    expect(target.reopen).not.toHaveBeenCalled()
    expect(target.deleteProvenTarget).toHaveBeenCalledWith(expect.objectContaining({
      candidate: input.candidate,
      trustedAction: true,
    }))
    expect(fixture.lifecycle.deleteOwnedTarget).toHaveBeenCalledOnce()
  })
})

function recoveryFixture(
  reopenOutcome?: Awaited<ReturnType<DirectZipTargetPort<Parent, FileHandle>['reopen']>>,
  suppliedTarget?: DirectZipTargetPort<Parent, FileHandle>,
  availableBytes = 1_000n,
) {
  const intent = {
    operationId: 'operation', digest: 'intent',
    plan: { kind: 'direct-resumable-zip', binding: { stableName: 'target.zip' } },
  } as unknown as ReceiveIntent
  const initial = lifecycle('resumable-receive', 1n)
  const lifecyclePort = {
    intent,
    lifecycle: initial,
    gate: vi.fn(async (decision: DirectZipLifecycleDecision) =>
      lifecycle(decision.kind, 2n)),
    activate: vi.fn(async () => Object.freeze({
      lifecycle: lifecycle('receiving', 2n),
      runtime: Object.freeze({ id: 'runtime' }),
    })),
    pause: vi.fn(async () => lifecycle('resumable-receive', 3n)),
    retain: vi.fn(async () => undefined),
    prepareCleanup: vi.fn(async () => undefined),
    prepareRetainedCleanup: vi.fn(async () => undefined),
    deleteOwnedTarget: vi.fn(async () => undefined),
  } satisfies DirectZipRecoveryLifecyclePort<FileHandle, Runtime>
  const target = suppliedTarget ?? targetSequence([reopenOutcome ?? readyReopen()])
  const input = {
    target,
    lifecycle: lifecyclePort,
    trustedAction: true,
    binding: {
      stableName: 'target.zip',
    },
    currentParent: { id: 'parent' },
    predecessor: {
      committedLength: 100n,
      observation: {},
      committedEpochs: [],
    },
    projectedArchiveLength: 200n,
    additionalTemporaryBytesUpperBound: 10n,
    space: { availableBytes: vi.fn(async () => availableBytes) },
  } as unknown as DirectZipRecoverySessionInput<Parent, FileHandle, Runtime>
  return { input, lifecycle: lifecyclePort, target }
}

function targetSequence(
  reopen: readonly Awaited<ReturnType<DirectZipTargetPort<Parent, FileHandle>['reopen']>>[],
  cleanup: Awaited<ReturnType<DirectZipTargetPort<Parent, FileHandle>['deleteProvenTarget']>> = {
    kind: 'ready', value: { disposition: 'deleted' },
  },
): DirectZipTargetPort<Parent, FileHandle> {
  const outcomes = [...reopen]
  return {
    reopen: vi.fn(async () => outcomes.shift() ?? readyReopen()),
    deleteProvenTarget: vi.fn(async () => cleanup),
    reserveBootstrap: vi.fn(),
    resumeBootstrap: vi.fn(),
    openEpoch: vi.fn(),
    truncateToPredecessor: vi.fn(),
  }
}

function readyReopen(): Awaited<ReturnType<DirectZipTargetPort<Parent, FileHandle>['reopen']>> {
  return {
    kind: 'ready',
    value: {
      resolution: { kind: 'replay-predecessor' },
      observation: {} as never,
      currentFile: { id: 'file' },
    },
  }
}

function gated(decision: DirectZipLifecycleDecision) {
  return { kind: 'gated' as const, decision }
}

function lifecycle(kind: ReceiveLifecycleState['kind'], generation: bigint): ReceiveLifecycleState {
  return { kind, operationId: 'operation', receiveIntentDigest: 'intent', generation } as ReceiveLifecycleState
}
