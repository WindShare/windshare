import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  reduceReceiveLifecycle,
  type LifecycleEvent,
} from '../../src/output/workspace/lifecycle'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  STABLE_RETENTION_MILLISECONDS,
  type ReceiveLifecycleState,
  type ReceiveLifecycleStatePayload,
} from '../../src/output/workspace/state'

const NOW = 10_000
const OPERATION_ID = identity(16, 1)
const INTENT_DIGEST = identity(32, 2)
const LEASE_ID = identity(16, 3)
const PACKAGE_DIGEST = identity(32, 4)

describe('origin-private aggregate lifecycle', () => {
  it('retries publication from WaitingToSave without rebuilding the package', () => {
    const expiresAt = NOW + STABLE_RETENTION_MILLISECONDS
    const waiting = state({ kind: 'waiting-to-save', packageDigest: PACKAGE_DIGEST, expiresAt })
    const first = apply(waiting, {
      kind: 'save-requested',
      publicationAttemptId: identity(16, 5),
    }, NOW + 1)
    const restored = apply(first, {
      kind: 'publication-not-committed',
      reason: 'user-cancelled',
    }, NOW + 2)
    const retried = apply(restored, {
      kind: 'save-requested',
      publicationAttemptId: identity(16, 6),
    }, NOW + 3)

    expect(restored).toEqual(expect.objectContaining({
      kind: 'waiting-to-save',
      packageDigest: PACKAGE_DIGEST,
      expiresAt: NOW + 2 + STABLE_RETENTION_MILLISECONDS,
    }))
    expect(retried).toEqual(expect.objectContaining({
      kind: 'publishing-managed',
      packageDigest: PACKAGE_DIGEST,
    }))
  })

  it('requires a confirmed managed commit before Published', () => {
    const publishing = state({
      kind: 'publishing-managed',
      packageDigest: PACKAGE_DIGEST,
      publicationAttemptId: identity(16, 5),
      activeLeaseId: LEASE_ID,
    })
    const published = apply(publishing, {
      kind: 'publication-committed',
      receiptDigest: identity(32, 7),
    })

    expect(published).toEqual(expect.objectContaining({
      kind: 'published',
      receiptDigest: identity(32, 7),
      cleanupState: 'cleanup-pending',
    }))
  })

  it('cleans a failed package attempt before resuming with a new object identity', () => {
    const sealedDigest = identity(32, 8)
    const packaging = state({
      kind: 'packaging',
      activeLeaseId: LEASE_ID,
      sealedMaterializationDigest: sealedDigest,
      packageTempObjectId: identity(32, 9),
    })
    const resumable = apply(packaging, {
      kind: 'package-retryable-failure',
      reason: 'writer-failed',
      tempCleanupProofDigest: identity(32, 10),
    })
    const resumed = apply(resumable, {
      kind: 'resume-started',
      packageTempObjectId: identity(32, 11),
    })

    expect(resumable.kind).toBe('resumable-package')
    expect(resumed).toEqual(expect.objectContaining({
      kind: 'packaging',
      sealedMaterializationDigest: sealedDigest,
      packageTempObjectId: identity(32, 11),
    }))
  })

  it('maps uncertain object ownership directly to NeedsAttention', () => {
    const receiving = state({ kind: 'receiving', activeLeaseId: LEASE_ID })
    const attention = apply(receiving, {
      kind: 'ownership-unknown',
      lastVerifiedRecordDigest: identity(32, 12),
    })

    expect(attention).toEqual(expect.objectContaining({
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
      lastVerifiedRecordDigest: identity(32, 12),
    }))
  })
})

function state(payload: ReceiveLifecycleStatePayload): ReceiveLifecycleState {
  return nextReceiveLifecycleState(initialReceiveLifecycleState({
    operationId: OPERATION_ID,
    receiveIntentDigest: INTENT_DIGEST,
  }), payload)
}

function apply(
  current: ReceiveLifecycleState,
  event: EventPayload,
  nowMilliseconds = NOW,
): ReceiveLifecycleState {
  return reduceReceiveLifecycle(current, {
    ...event,
    expectedGeneration: current.generation,
    leaseId: LEASE_ID,
  } as Parameters<typeof reduceReceiveLifecycle>[1], {
    planKind: 'workspace-then-publish',
    preparationRequired: false,
    nowMilliseconds,
    activeLeaseId: LEASE_ID,
  }).state
}

type EventPayload = LifecycleEvent extends infer Event
  ? Event extends LifecycleEvent
    ? Omit<Event, 'expectedGeneration' | 'leaseId'>
    : never
  : never

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
