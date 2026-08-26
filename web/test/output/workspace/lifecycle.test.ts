import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import { reduceReceiveLifecycle } from '../../../src/output/workspace/lifecycle'
import {
  STABLE_RETENTION_MILLISECONDS,
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type PlanKind,
  type ReceiveLifecycleState,
} from '../../../src/output/workspace/state'

const NOW = 1_000_000
const LEASE = identity(16, 7)
const SELECTION_FACTS = Object.freeze({
  discoveredFileCount: 3n,
  discoveredBytes: 40n,
  discovery: 'failed' as const,
})

describe('receive lifecycle reducer', () => {
  it('starts the 24-hour clock only after verified pause and clears it on resume', () => {
    const initial = initialReceiveLifecycleState({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
    })
    const receiving = reduceReceiveLifecycle(initial, {
      kind: 'receive-started',
      expectedGeneration: 1n,
      leaseId: LEASE,
    }, context('direct-tree', NOW)).state
    const stable = reduceReceiveLifecycle(receiving, {
      kind: 'pause-verified',
      stage: 'receive',
      expectedGeneration: receiving.generation,
      leaseId: LEASE,
      checkpointSetDigest: identity(32, 3),
      completedFileCount: 1n,
      completedBytes: 12n,
      selectionFacts: SELECTION_FACTS,
    }, context('direct-tree', NOW)).state

    expect(stable).toEqual(expect.objectContaining({
      kind: 'resumable-receive',
      expiresAt: NOW + STABLE_RETENTION_MILLISECONDS,
    }))

    const stale = reduceReceiveLifecycle(stable, {
      kind: 'resume-started',
      expectedGeneration: stable.generation - 1n,
      leaseId: LEASE,
    }, context('direct-tree', NOW + 500))
    expect(stale).toEqual({ status: 'stale', state: stable })

    const resumed = reduceReceiveLifecycle(stable, {
      kind: 'resume-started',
      expectedGeneration: stable.generation,
      leaseId: LEASE,
    }, context('direct-tree', NOW + 500)).state
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
      expiresAt: NOW + STABLE_RETENTION_MILLISECONDS,
    }, context('direct-tree', NOW + 600)).state
    expect(restored).toEqual(expect.objectContaining({
      kind: 'resumable-receive',
      completedFileCount: 1n,
      completedBytes: 12n,
      selectionFacts: SELECTION_FACTS,
      expiresAt: NOW + STABLE_RETENTION_MILLISECONDS,
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
    }, context('workspace-then-publish', NOW))

    expect(reduced.state).toEqual(expect.objectContaining({
      kind: 'needs-attention',
      reason: 'publication-unknown',
      lastVerifiedRecordDigest: identity(32, 6),
    }))
  })

  it('preserves the original workspace handoff deadline into DownloadStarted', () => {
    const deadline = NOW + STABLE_RETENTION_MILLISECONDS
    const waiting = state({
      kind: 'waiting-to-save',
      packageDigest: identity(32, 4),
      expiresAt: deadline,
    })
    const handingOff = reduceReceiveLifecycle(waiting, {
      kind: 'handoff-requested',
      expectedGeneration: waiting.generation,
      leaseId: LEASE,
      attemptKind: 'workspace',
      attemptId: identity(16, 5),
    }, context('workspace-then-publish', NOW + 100)).state
    const downloaded = reduceReceiveLifecycle(handingOff, {
      kind: 'handoff-started',
      expectedGeneration: handingOff.generation,
      leaseId: LEASE,
    }, context('workspace-then-publish', NOW + 200)).state

    expect(handingOff).toEqual(expect.objectContaining({ retainedDeadline: deadline }))
    expect(downloaded).toEqual(expect.objectContaining({ retryableUntil: deadline }))
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
    }, context('workspace-then-publish', NOW))).toThrow('exclusive to DirectTree')
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
      context('direct-tree', NOW, true),
    )).toThrow('direct plans')
    expect(() => reduceReceiveLifecycle(
      initial,
      {
        kind: 'receive-started',
        expectedGeneration: initial.generation,
        leaseId: LEASE,
      },
      context('portable-handoff', NOW),
    )).toThrow('requires sealed preparation')
    expect(reduceReceiveLifecycle(
      initial,
      started,
      context('portable-handoff', NOW, true),
    ).state.kind).toBe('preparing')
  })

  it('expires at the exact stable deadline and never during inspection before it', () => {
    const deadline = NOW + STABLE_RETENTION_MILLISECONDS
    const waiting = state({
      kind: 'waiting-to-save',
      packageDigest: identity(32, 4),
      expiresAt: deadline,
    })
    const event = {
      kind: 'expiry-observed' as const,
      expectedGeneration: waiting.generation,
      leaseId: LEASE,
      expiryReceiptDigest: identity(32, 5),
      cleanupState: 'cleanup-pending' as const,
    }

    expect(reduceReceiveLifecycle(
      waiting,
      event,
      context('workspace-then-publish', deadline - 1),
    )).toEqual({ status: 'not-due', state: waiting })
    const expired = reduceReceiveLifecycle(
      waiting,
      event,
      context('workspace-then-publish', deadline),
    ).state
    expect(expired.kind).toBe('expired')

    const cleaned = reduceReceiveLifecycle(expired, {
      kind: 'cleanup-verified',
      expectedGeneration: expired.generation,
      leaseId: LEASE,
      cleanupReceiptDigest: identity(32, 6),
    }, context('workspace-then-publish', deadline + 1)).state
    expect(cleaned).toEqual(expect.objectContaining({
      kind: 'expired',
      expiresAt: deadline,
      cleanupState: 'clean',
    }))
  })

  it('forbids stable-state continuation at the exact deadline', () => {
    const deadline = NOW + STABLE_RETENTION_MILLISECONDS
    const waiting = state({
      kind: 'waiting-to-save',
      packageDigest: identity(32, 4),
      expiresAt: deadline,
    })

    expect(() => reduceReceiveLifecycle(waiting, {
      kind: 'save-requested',
      expectedGeneration: waiting.generation,
      leaseId: LEASE,
      publicationAttemptId: identity(16, 5),
    }, context('workspace-then-publish', deadline))).toThrow('deadline elapsed')
    expect(() => reduceReceiveLifecycle(waiting, {
      kind: 'handoff-requested',
      expectedGeneration: waiting.generation,
      leaseId: LEASE,
      attemptKind: 'workspace',
      attemptId: identity(16, 6),
    }, context('workspace-then-publish', deadline))).toThrow('deadline elapsed')
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
    }, context('portable-handoff', NOW, true))).toThrow('WorkspaceThenPublish')
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
  nowMilliseconds: number,
  preparationRequired = false,
) {
  return {
    planKind,
    preparationRequired,
    activeLeaseId: LEASE,
    nowMilliseconds,
  } as const
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
