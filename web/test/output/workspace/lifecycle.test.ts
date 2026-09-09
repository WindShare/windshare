import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import { canonicalReceiveLifecycleStateBytes, decodeReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import { reduceReceiveLifecycle } from '../../../src/output/workspace/lifecycle'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type PlanKind,
  type ReceiveLifecycleState,
} from '../../../src/output/workspace/state'

const LEASE = identity(16, 7)
const SELECTION_FACTS = Object.freeze({
  discoveredFileCount: 3n,
  discoveredBytes: 40n,
  discovery: 'failed' as const,
})

describe('receive lifecycle reducer', () => {
  it('resumes native ZIP checkpoints only in their workspace plan', () => {
    const paused = nextReceiveLifecycleState(initialReceiveLifecycleState({
      operationId: identity(16, 1), receiveIntentDigest: identity(32, 2),
    }), {
      kind: 'resumable-receive', payloadKind: 'opfs-zip', objectId: identity(32, 9),
      checkpointGeneration: 4n, completedFileCount: 1n, completedBytes: 12n,
      discoveryComplete: false, occupiedBytes: 100n, pauseReason: 'storage-pressure',
    })
    expect(decodeReceiveLifecycleState(canonicalReceiveLifecycleStateBytes(paused))).toEqual(paused)
    const event = { kind: 'resume-started' as const, expectedGeneration: paused.generation, leaseId: LEASE }
    expect(reduceReceiveLifecycle(paused, event, context('workspace-then-publish')).state)
      .toMatchObject({ kind: 'receiving', activeLeaseId: LEASE })
    expect(() => reduceReceiveLifecycle(paused, event, context('direct-tree')))
      .toThrow('plan cannot resume')
  })

  it('retains verified pause evidence without an automatic deadline', () => {
    const initial = initialReceiveLifecycleState({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
    })
    const receiving = reduceReceiveLifecycle(initial, {
      kind: 'receive-started',
      expectedGeneration: 1n,
      leaseId: LEASE,
    }, context('direct-tree')).state
    const stable = reduceReceiveLifecycle(receiving, {
      kind: 'pause-verified',
      stage: 'receive',
      expectedGeneration: receiving.generation,
      leaseId: LEASE,
      checkpointSetDigest: identity(32, 3),
      completedFileCount: 1n,
      completedBytes: 12n,
      selectionFacts: SELECTION_FACTS,
    }, context('direct-tree')).state

    expect(stable).toEqual(expect.objectContaining({
      kind: 'resumable-receive',
    }))

    const stale = reduceReceiveLifecycle(stable, {
      kind: 'resume-started',
      expectedGeneration: stable.generation - 1n,
      leaseId: LEASE,
    }, context('direct-tree'))
    expect(stale).toEqual({ status: 'stale', state: stable })

    const resumed = reduceReceiveLifecycle(stable, {
      kind: 'resume-started',
      expectedGeneration: stable.generation,
      leaseId: LEASE,
    }, context('direct-tree')).state
    expect(resumed.kind).toBe('receiving')
    expect(resumed).not.toHaveProperty('expiresAt')

    const restored = reduceReceiveLifecycle(resumed, {
      kind: 'resume-admission-failed',
      expectedGeneration: resumed.generation,
      leaseId: LEASE,
      checkpointSetDigest: stable.kind === 'resumable-receive' && stable.payloadKind === 'file-set'
        ? stable.checkpointSetDigest
        : identity(32, 4),
      completedFileCount: 1n,
      completedBytes: 12n,
      selectionFacts: SELECTION_FACTS,
    }, context('direct-tree')).state
    expect(restored).toEqual(expect.objectContaining({
      kind: 'resumable-receive',
      completedFileCount: 1n,
      completedBytes: 12n,
      selectionFacts: SELECTION_FACTS,
    }))
  })

  it('maps unknown external outcomes to closed NeedsAttention reasons', () => {
    const publishing = state({
      kind: 'publishing-managed',
      activeLeaseId: LEASE,
      packageDigest: identity(32, 4),
      publicationAttemptId: identity(16, 5),
    })
    const reduced = reduceReceiveLifecycle(publishing, {
      kind: 'publication-unknown',
      expectedGeneration: publishing.generation,
      leaseId: LEASE,
      lastVerifiedRecordDigest: identity(32, 6),
    }, context('workspace-then-publish'))

    expect(reduced.state).toEqual(expect.objectContaining({
      kind: 'needs-attention',
      reason: 'publication-unknown',
      lastVerifiedRecordDigest: identity(32, 6),
    }))
  })

  it('retains artifact identity independently of browser save attempts', () => {
    const waiting = state({
      kind: 'waiting-to-save',
      packageDigest: identity(32, 4),
    })
    const handingOff = reduceReceiveLifecycle(waiting, {
      kind: 'handoff-requested',
      expectedGeneration: waiting.generation,
      leaseId: LEASE,
      attemptKind: 'workspace',
      attemptId: identity(16, 5),
    }, context('workspace-then-publish')).state
    const downloaded = reduceReceiveLifecycle(handingOff, {
      kind: 'handoff-started',
      expectedGeneration: handingOff.generation,
      leaseId: LEASE,
    }, context('workspace-then-publish')).state

    expect(handingOff).not.toHaveProperty('retainedDeadline')
    expect(downloaded).toMatchObject({ kind: 'download-started', packageDigest: waiting.kind === 'waiting-to-save' ? waiting.packageDigest : '' })
    expect(downloaded).not.toHaveProperty('retryableUntil')
  })

  it('keeps Stop exclusive to DirectTree', () => {
    const receiving = state({ kind: 'receiving', activeLeaseId: LEASE })
    expect(() => reduceReceiveLifecycle(receiving, {
      kind: 'stop-requested',
      expectedGeneration: receiving.generation,
      leaseId: LEASE,
      successCount: 0n,
      failureCount: 0n,
      receiptDigest: identity(32, 8),
      cleanupReceiptDigest: identity(32, 9),
    }, context('workspace-then-publish'))).toThrow('exclusive to DirectTree')
  })

  it('keeps preparation content gates plan-specific', () => {
    const initial = initialReceiveLifecycleState({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
    })
    const started = {
      kind: 'receive-started' as const,
      expectedGeneration: initial.generation,
      leaseId: LEASE,
      preparationId: identity(16, 3),
    }

    expect(() => reduceReceiveLifecycle(
      initial,
      started,
      context('direct-tree', true),
    )).toThrow('direct plans')
    expect(() => reduceReceiveLifecycle(
      initial,
      {
        kind: 'receive-started',
        expectedGeneration: initial.generation,
        leaseId: LEASE,
      },
      context('portable-handoff'),
    )).toThrow('requires sealed preparation')
    expect(reduceReceiveLifecycle(
      initial,
      started,
      context('portable-handoff', true),
    ).state.kind).toBe('preparing')
  })

  it('allows saving and repeated handoff for retained artifacts', () => {
    const waiting = state({
      kind: 'waiting-to-save',
      packageDigest: identity(32, 4),
    })

    expect(() => reduceReceiveLifecycle(waiting, {
      kind: 'save-requested',
      expectedGeneration: waiting.generation,
      leaseId: LEASE,
      publicationAttemptId: identity(16, 5),
    }, context('workspace-then-publish'))).not.toThrow()
    expect(() => reduceReceiveLifecycle(waiting, {
      kind: 'handoff-requested',
      expectedGeneration: waiting.generation,
      leaseId: LEASE,
      attemptKind: 'workspace',
      attemptId: identity(16, 6),
    }, context('workspace-then-publish'))).not.toThrow()
  })

  it.each([
    { kind: 'partial-directory', reason: 'stopped', successCount: 1n, failureCount: 0n, receiptDigest: identity(32, 4) },
    { kind: 'restart-required', reason: 'portable-aborted', receiptDigest: identity(32, 4) },
  ] as const)('preserves $kind when cleanup is observed', payload => {
    const terminal = state(payload)
    expect(() => reduceReceiveLifecycle(terminal, {
      kind: 'cleanup-verified',
      expectedGeneration: terminal.generation,
      leaseId: LEASE,
      cleanupReceiptDigest: identity(32, 5),
    }, context('direct-tree'))).toThrow('cannot erase a retained terminal outcome')
  })

  it('never persists an unknown Portable handoff outcome', () => {
    const handingOff = state({
      kind: 'handing-off',
      activeLeaseId: LEASE,
      attemptKind: 'portable',
      attemptId: identity(16, 4),
    })
    expect(() => reduceReceiveLifecycle(handingOff, {
      kind: 'handoff-unknown',
      expectedGeneration: handingOff.generation,
      leaseId: LEASE,
      lastVerifiedRecordDigest: identity(32, 5),
    }, context('portable-handoff', true))).toThrow('WorkspaceThenPublish')
  })
})

function state(
  payload: Parameters<typeof nextReceiveLifecycleState>[1],
): ReceiveLifecycleState {
  const initial = initialReceiveLifecycleState({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
  })
  return nextReceiveLifecycleState(initial, payload)
}

function context(
  planKind: PlanKind,
  preparationRequired = false,
) {
  return {
    planKind,
    preparationRequired,
    activeLeaseId: LEASE,
  } as const
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
