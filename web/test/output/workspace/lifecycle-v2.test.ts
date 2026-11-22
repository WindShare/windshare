import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import { canonicalRecord } from '../../../src/transfer/encoding/canonical'
import { reduceReceiveLifecycle } from '../../../src/transfer/lifecycle/reducer'
import {
  isTerminalLifecycleState,
  type ReceiveLifecycleState,
} from '../../../src/transfer/lifecycle/state'
import {
  canonicalReceiveLifecycleStateBytes,
  decodeReceiveLifecycleState,
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../../../src/output/workspace/state-codec'

describe('receive lifecycle V2 durable states', () => {
  it.each(['paused', 'failed'] as const)('persists %s startup without a payload checkpoint and allows explicit discard', async reason => {
    const state: ReceiveLifecycleState = { ...base(), kind: 'resumable-start', reason }
    const restored = decodeStoredReceiveLifecycleState(await storedReceiveLifecycleState(state))
    expect(restored).toEqual(state)
    expect(isTerminalLifecycleState(restored)).toBe(false)
    expect(reduceReceiveLifecycle(restored, {
      kind: 'cleanup-verified', expectedGeneration: restored.generation,
      leaseId: identity(16, 2), cleanupReceiptDigest: identity(32, 10),
    }, { planKind: 'workspace-then-publish', activeLeaseId: identity(16, 2), preparationRequired: false }).state.kind).toBe('discarded')
  })

  it('persists source invalidation as a terminal receive with explicit retained-data cleanup', async () => {
    const receiving: ReceiveLifecycleState = { ...base(), kind: 'receiving', activeLeaseId: identity(16, 2) }
    const context = { planKind: 'workspace-then-publish' as const, activeLeaseId: identity(16, 2), preparationRequired: false }
    const invalidated = reduceReceiveLifecycle(receiving, {
      kind: 'source-invalidation-verified', expectedGeneration: receiving.generation,
      leaseId: context.activeLeaseId, checkpointSetDigest: identity(32, 9),
    }, context).state
    expect(invalidated.kind).toBe('source-invalidated')
    expect(isTerminalLifecycleState(invalidated)).toBe(true)
    const restored = decodeStoredReceiveLifecycleState(await storedReceiveLifecycleState(invalidated))
    expect(restored).toEqual(invalidated)
    expect(() => reduceReceiveLifecycle(restored, {
      kind: 'resume-started', expectedGeneration: restored.generation, leaseId: context.activeLeaseId,
    }, context)).toThrow()
    expect(reduceReceiveLifecycle(restored, {
      kind: 'cleanup-verified', expectedGeneration: restored.generation,
      leaseId: context.activeLeaseId, cleanupReceiptDigest: identity(32, 10),
    }, context).state.kind).toBe('discarded')
  })

  it('round-trips pause selection facts in the same canonical lifecycle record', async () => {
    const lifecycle: ReceiveLifecycleState = Object.freeze({
      ...base(),
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(32, 9),
      completedFileCount: 2n,
      retainedBytes: 12n,
      completedBytes: 12n,
      selectionFacts: Object.freeze({
        discoveredFileCount: 5n,
        discoveredBytes: 90n,
        discovery: 'failed',
      }),
    })

    const canonical = canonicalReceiveLifecycleStateBytes(lifecycle)
    expect(decodeReceiveLifecycleState(canonical)).toEqual(lifecycle)
    expect(decodeStoredReceiveLifecycleState(await storedReceiveLifecycleState(lifecycle)))
      .toEqual(lifecycle)
  })

  it('rejects selection facts that cannot contain completed output', () => {
    const lifecycle: ReceiveLifecycleState = Object.freeze({
      ...base(),
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(32, 9),
      completedFileCount: 2n,
      retainedBytes: 12n,
      completedBytes: 12n,
      selectionFacts: Object.freeze({
        discoveredFileCount: 1n,
        discoveredBytes: 11n,
        discovery: 'complete',
      }),
    })

    expect(() => canonicalReceiveLifecycleStateBytes(lifecycle)).toThrow(/do not contain completed output/)
  })

  it('rejects V1 lifecycle bytes instead of guessing a payload interpretation', () => {
    const legacy = canonicalRecord('windshare/receive-lifecycle-state/v1', 1, [])
    expect(() => decodeReceiveLifecycleState(legacy)).toThrow()
  })
})

function base() {
  return Object.freeze({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    generation: 2n,
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
