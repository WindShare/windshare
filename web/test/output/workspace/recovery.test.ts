import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import { recoverAbandonedOperation } from '../../../src/output/workspace/recovery'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type PlanKind,
  type ReceiveLifecycleState,
} from '../../../src/output/workspace/state'
import type { ReceiveOperationTraceEvent } from '../../../src/output/workspace/trace'

const OBSERVATION_INTERVAL = 86_400_000
const NOW = 50_000

describe('abandoned receive recovery', () => {
  it('uses the injected clock at a prepared crash cut', () => {
    const preparing = state({
      kind: 'preparing',
      preparationId: identity(16, 3),
    })
    const reduction = recoverAbandonedOperation(preparing, {
      kind: 'verified-receive',
      checkpointSetDigest: identity(32, 4),
      completedFileCount: 2n,
      completedBytes: 10n,
      selectionFacts: Object.freeze({
        discoveredFileCount: 4n,
        discoveredBytes: 30n,
        discovery: 'failed' as const,
      }),
      lastVerifiedRecordDigest: identity(32, 5),
    }, context('workspace-then-publish', NOW))

    expect(reduction).toEqual(expect.objectContaining({
      decision: 'resume-receive',
      state: expect.objectContaining({
        kind: 'resumable-receive',
        selectionFacts: {
          discoveredFileCount: 4n,
          discoveredBytes: 30n,
          discovery: 'failed',
        },
      }),
    }))
  })

  it.each([
    ['target', 'target-ownership-unknown'],
    ['publication', 'publication-unknown'],
    ['cleanup', 'cleanup-unknown'],
  ] as const)('maps unknown %s ownership to NeedsAttention(%s)', (authority, reason) => {
    const receiving = state({
      kind: 'receiving',
      activeLeaseId: identity(16, 6),
    })
    const events: ReceiveOperationTraceEvent[] = []
    const reduction = recoverAbandonedOperation(receiving, {
      kind: 'unknown',
      authority,
      lastVerifiedRecordDigest: identity(32, 5),
    }, {
      ...context('direct-tree', NOW),
      onTrace: (event) => events.push(event),
    })

    expect(reduction.state).toEqual(expect.objectContaining({
      kind: 'needs-attention',
      reason,
    }))
    expect(JSON.stringify(events)).not.toContain('canonicalPath')
    expect(events.at(-1)).toEqual(expect.objectContaining({
      name: 'receive.operation.needs_attention',
      context: expect.objectContaining({ needs_attention_reason: reason }),
    }))
  })

  it('preserves unfinished package recovery after long retention', () => {
    const expiresAt = NOW + OBSERVATION_INTERVAL
    const stable = state({
      kind: 'materialization-sealed',
      sealedMaterializationDigest: identity(32, 4),
    })
    const reduction = recoverAbandonedOperation(stable, {
      kind: 'verified-package',
      sealedMaterializationDigest: identity(32, 4),
      tempCleanupProofDigest: identity(32, 5),
      lastVerifiedRecordDigest: identity(32, 6),
    }, context('workspace-then-publish', expiresAt))

    expect(reduction.decision).toBe('resume-package')
    expect(reduction.state).toEqual(expect.objectContaining({
      kind: 'resumable-package',
    }))
  })

  it('does not retry an unknown atomic commit', () => {
    const committing = state({
      kind: 'committing-atomic',
      activeLeaseId: identity(16, 6),
    })
    const reduction = recoverAbandonedOperation(committing, {
      kind: 'atomic-commit',
      outcome: 'unknown',
      receiptDigest: identity(32, 7),
      lastVerifiedRecordDigest: identity(32, 8),
    }, context('direct-atomic', NOW))

    expect(reduction.state).toEqual(expect.objectContaining({
      kind: 'needs-attention',
      reason: 'publication-unknown',
    }))
  })

  it('requires recovered package evidence to name the exact materialization seal', () => {
    const sealed = state({
      kind: 'materialization-sealed',
      sealedMaterializationDigest: identity(32, 4),
    })
    const reduction = recoverAbandonedOperation(sealed, {
      kind: 'verified-package',
      sealedMaterializationDigest: identity(32, 7),
      tempCleanupProofDigest: identity(32, 5),
      lastVerifiedRecordDigest: identity(32, 6),
    }, context('workspace-then-publish', NOW))

    expect(reduction.state).toEqual(expect.objectContaining({
      kind: 'needs-attention',
      reason: 'cleanup-unknown',
      lastVerifiedRecordDigest: identity(32, 6),
    }))
  })

  it('recovers sealed artifact and workspace handoff crash cuts deterministically', () => {
    const artifact = state({
      kind: 'artifact-sealed',
      packageDigest: identity(32, 4),
    })
    const waiting = recoverAbandonedOperation(artifact, {
      kind: 'verified-artifact',
      packageDigest: identity(32, 4),
      lastVerifiedRecordDigest: identity(32, 5),
    }, context('workspace-then-publish', NOW)).state
    expect(waiting).toEqual(expect.objectContaining({
      kind: 'waiting-to-save',
    }))

    const handingOff = nextReceiveLifecycleState(waiting, {
      kind: 'handing-off',
      activeLeaseId: identity(16, 6),
      attemptKind: 'workspace',
      attemptId: identity(16, 7),
      packageDigest: identity(32, 4),
    })
    const downloaded = recoverAbandonedOperation(handingOff, {
      kind: 'handoff',
      outcome: 'started',
      lastVerifiedRecordDigest: identity(32, 8),
      expiryReceiptDigest: identity(32, 9),
    }, context('workspace-then-publish', NOW + 1)).state
    expect(downloaded).toEqual(expect.objectContaining({
      kind: 'download-started',
    }))
  })

  it('rejects durable recovery for runtime-only Portable lifecycle', () => {
    const preparing = state({
      kind: 'preparing',
      preparationId: identity(16, 3),
    })
    expect(() => recoverAbandonedOperation(preparing, {
      kind: 'restart-preparation',
      preparationId: identity(16, 4),
      cleanupReceiptDigest: identity(32, 5),
      lastVerifiedRecordDigest: identity(32, 6),
    }, context('portable-handoff', NOW))).toThrow('runtime-only')
  })
})

function state(
  payload: Parameters<typeof nextReceiveLifecycleState>[1],
): ReceiveLifecycleState {
  return nextReceiveLifecycleState(initialReceiveLifecycleState({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
  }), payload)
}

function context(
  planKind: PlanKind,
  nowMilliseconds: number,
) {
  return {
    planKind,
    nowMilliseconds,
    expiryReceiptDigest: identity(32, 9),
  } as const
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
