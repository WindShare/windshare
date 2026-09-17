import { describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../../src/output/workspace/state'
import { WorkspaceExecutionAdmissionSettlement } from '../../src/ui/browser-receive/workspace-admission'

describe('workspace execution admission settlement', () => {
  it('restores the exact continuation when execution was not admitted', async () => {
    const states = continuationStates()
    let current: ReceiveLifecycleState = states.receiving
    const restoreContinuation = vi.fn(async () => {
      current = states.restored
      return states.restored
    })
    const retainStart = vi.fn(async () => Object.freeze({
      lifecycle: nextReceiveLifecycleState(states.receiving, { kind: 'resumable-start', reason: 'failed' }),
      workspaceUsage: null,
    }))
    const recordUnknown = vi.fn(async () => needsAttention(states.receiving))
    const settlement = new WorkspaceExecutionAdmissionSettlement({
      operationId: states.receiving.operationId,
      currentLifecycle: async () => current,
      retainStart,
      recordUnknown,
      workspaceUsage,
    }, { kind: 'continuation', restore: () => restoreContinuation() })

    await expect(settlement.settle()).resolves.toEqual({
      lifecycle: states.restored,
      workspaceUsage: workspaceUsage(states.restored),
    })
    expect(restoreContinuation).toHaveBeenCalledOnce()
    expect(retainStart).not.toHaveBeenCalled()
    expect(recordUnknown).not.toHaveBeenCalled()
  })

  it('records NeedsAttention instead of rolling back an admitted execution', async () => {
    const states = continuationStates()
    const restoreContinuation = vi.fn(async () => states.restored)
    const retainStart = vi.fn(async () => Object.freeze({
      lifecycle: nextReceiveLifecycleState(states.receiving, { kind: 'resumable-start', reason: 'failed' }),
      workspaceUsage: null,
    }))
    const attention = needsAttention(states.receiving)
    const recordUnknown = vi.fn(async () => attention)
    const settlement = new WorkspaceExecutionAdmissionSettlement({
      operationId: states.receiving.operationId,
      currentLifecycle: async () => states.receiving,
      retainStart,
      recordUnknown,
      workspaceUsage,
    }, { kind: 'continuation', restore: () => restoreContinuation() })
    settlement.markExecutionAdmitted()

    await expect(settlement.settle()).resolves.toEqual({
      lifecycle: attention,
      workspaceUsage: workspaceUsage(attention),
    })
    expect(restoreContinuation).not.toHaveBeenCalled()
    expect(retainStart).not.toHaveBeenCalled()
    expect(recordUnknown).toHaveBeenCalledOnce()
  })

  it('retains failed startup once and admits a new attempt only on an explicit retry', async () => {
    const initial = initialReceiveLifecycleState({
      operationId: identity(1, 16),
      receiveIntentDigest: identity(2, 32),
    })
    const retained = nextReceiveLifecycleState(initial, { kind: 'resumable-start', reason: 'failed' })
    const retainStart = vi.fn(async () => Object.freeze({ lifecycle: retained, workspaceUsage: null }))
    const settlement = new WorkspaceExecutionAdmissionSettlement({
      operationId: initial.operationId,
      currentLifecycle: async () => initial,
      retainStart,
      recordUnknown: async () => needsAttention(initial),
      workspaceUsage,
    }, { kind: 'fresh' })

    const failure = new Error('output initialization failed')
    await expect(settlement.settle(failure)).resolves.toEqual({
      lifecycle: retained,
      workspaceUsage: null,
    })
    await settlement.settle(failure)
    expect(retainStart).toHaveBeenCalledExactlyOnceWith(failure)
    settlement.beginStart()
    await settlement.settle(failure)
    expect(retainStart).toHaveBeenCalledTimes(2)
  })
})

function continuationStates() {
  const initial = initialReceiveLifecycleState({
    operationId: identity(3, 16),
    receiveIntentDigest: identity(4, 32),
  })
  const fallback = nextReceiveLifecycleState(initial, {
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    checkpointSetDigest: identity(5, 32),
    completedFileCount: 19n,
    retainedBytes: 35_020n,
    completedBytes: 35_020n,
    selectionFacts: Object.freeze({
      discoveredFileCount: 19n,
      discoveredBytes: 35_020n,
      discovery: 'complete',
    }),
  })
  if (fallback.kind !== 'resumable-receive' || fallback.payloadKind !== 'file-set') {
    throw new Error('test fallback changed payload kind')
  }
  const receiving = nextReceiveLifecycleState(fallback, {
    kind: 'receiving',
    activeLeaseId: identity(6, 16),
  })
  const restored = nextReceiveLifecycleState(receiving, {
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    checkpointSetDigest: fallback.checkpointSetDigest,
    completedFileCount: fallback.completedFileCount,
    retainedBytes: fallback.completedBytes,
    completedBytes: fallback.completedBytes,
    selectionFacts: fallback.selectionFacts,
  })
  if (restored.kind !== 'resumable-receive' || restored.payloadKind !== 'file-set') {
    throw new Error('test restoration changed payload kind')
  }
  return Object.freeze({ fallback, receiving, restored })
}

function needsAttention(state: ReceiveLifecycleState) {
  const attention = nextReceiveLifecycleState(state, {
    kind: 'needs-attention',
    reason: 'target-ownership-unknown',
    lastVerifiedRecordDigest: identity(8, 32),
  })
  if (attention.kind !== 'needs-attention') throw new Error('test attention changed kind')
  return attention
}

function workspaceUsage(state: ReceiveLifecycleState) {
  return Object.freeze({
    ownedBytes:
      state.kind === 'resumable-receive' && state.payloadKind === 'file-set'
        ? state.completedBytes
        : 0n,
    maximumBytes: 1_000_000n,
  })
}

function identity(seed: number, width: number): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[width - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}
