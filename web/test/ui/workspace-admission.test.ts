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
    const discard = vi.fn(async () => Object.freeze({
      lifecycle: discarded(states.receiving),
      workspaceUsage: null,
    }))
    const recordUnknown = vi.fn(async () => needsAttention(states.receiving))
    const settlement = new WorkspaceExecutionAdmissionSettlement({
      operationId: states.receiving.operationId,
      currentLifecycle: async () => current,
      restoreContinuation,
      discard,
      recordUnknown,
      workspaceUsage,
    }, states.fallback)

    await expect(settlement.settle()).resolves.toEqual({
      lifecycle: states.restored,
      workspaceUsage: workspaceUsage(states.restored),
    })
    expect(restoreContinuation).toHaveBeenCalledWith(states.fallback)
    expect(discard).not.toHaveBeenCalled()
    expect(recordUnknown).not.toHaveBeenCalled()
  })

  it('records NeedsAttention instead of rolling back an admitted execution', async () => {
    const states = continuationStates()
    const restoreContinuation = vi.fn(async () => states.restored)
    const discard = vi.fn(async () => Object.freeze({
      lifecycle: discarded(states.receiving),
      workspaceUsage: null,
    }))
    const attention = needsAttention(states.receiving)
    const recordUnknown = vi.fn(async () => attention)
    const settlement = new WorkspaceExecutionAdmissionSettlement({
      operationId: states.receiving.operationId,
      currentLifecycle: async () => states.receiving,
      restoreContinuation,
      discard,
      recordUnknown,
      workspaceUsage,
    }, states.fallback)
    settlement.markExecutionAdmitted()

    await expect(settlement.settle()).resolves.toEqual({
      lifecycle: attention,
      workspaceUsage: workspaceUsage(attention),
    })
    expect(restoreContinuation).not.toHaveBeenCalled()
    expect(discard).not.toHaveBeenCalled()
    expect(recordUnknown).toHaveBeenCalledOnce()
  })

  it('discards a fresh operation that failed before execution admission', async () => {
    const initial = initialReceiveLifecycleState({
      operationId: identity(1, 16),
      receiveIntentDigest: identity(2, 32),
    })
    const terminal = discarded(initial)
    const discard = vi.fn(async () => Object.freeze({ lifecycle: terminal, workspaceUsage: null }))
    const settlement = new WorkspaceExecutionAdmissionSettlement({
      operationId: initial.operationId,
      currentLifecycle: async () => initial,
      restoreContinuation: async fallback => fallback,
      discard,
      recordUnknown: async () => needsAttention(initial),
      workspaceUsage,
    })

    await expect(settlement.settle()).resolves.toEqual({
      lifecycle: terminal,
      workspaceUsage: null,
    })
    expect(discard).toHaveBeenCalledOnce()
  })
})

function continuationStates() {
  const initial = initialReceiveLifecycleState({
    operationId: identity(3, 16),
    receiveIntentDigest: identity(4, 32),
  })
  const fallback = nextReceiveLifecycleState(initial, {
    kind: 'resumable-receive',
    checkpointSetDigest: identity(5, 32),
    completedFileCount: 19n,
    completedBytes: 35_020n,
    expiresAt: 86_401_000,
  })
  if (fallback.kind !== 'resumable-receive') throw new Error('test fallback changed kind')
  const receiving = nextReceiveLifecycleState(fallback, {
    kind: 'receiving',
    activeLeaseId: identity(6, 16),
  })
  const restored = nextReceiveLifecycleState(receiving, {
    kind: 'resumable-receive',
    checkpointSetDigest: fallback.checkpointSetDigest,
    completedFileCount: fallback.completedFileCount,
    completedBytes: fallback.completedBytes,
    expiresAt: fallback.expiresAt,
  })
  if (restored.kind !== 'resumable-receive') throw new Error('test restoration changed kind')
  return Object.freeze({ fallback, receiving, restored })
}

function discarded(state: ReceiveLifecycleState) {
  const terminal = nextReceiveLifecycleState(state, {
    kind: 'discarded',
    cleanupReceiptDigest: identity(7, 32),
  })
  if (terminal.kind !== 'discarded') throw new Error('test terminal changed kind')
  return terminal
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
    ownedBytes: state.kind === 'resumable-receive' ? state.completedBytes : 0n,
    maximumBytes: 1_000_000n,
  })
}

function identity(seed: number, width: number): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[width - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}
