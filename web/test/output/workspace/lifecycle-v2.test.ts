import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createDirectZipRecoveryGateV1,
  decodeDirectZipRecoveryGateV1,
} from '../../../src/output/direct-zip/journal/recovery-gate'
import { canonicalRecord } from '../../../src/output/workspace/canonical'
import { reduceReceiveLifecycle } from '../../../src/output/workspace/lifecycle'
import {
  RECEIVE_STATE_AUTHORIZATION_REQUIRED,
  RECEIVE_STATE_DESTINATION_SPACE_REQUIRED,
  RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED,
  receiveStateByte,
  initialReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../../../src/output/workspace/state'
import {
  canonicalReceiveLifecycleStateBytes,
  decodeReceiveLifecycleState,
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../../../src/output/workspace/state-codec'

describe('receive lifecycle V2 durable states', () => {
  it('round-trips pause selection facts in the same canonical lifecycle record', async () => {
    const lifecycle: ReceiveLifecycleState = Object.freeze({
      ...base(),
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(32, 9),
      completedFileCount: 2n,
      completedBytes: 12n,
      selectionFacts: Object.freeze({
        discoveredFileCount: 5n,
        discoveredBytes: 90n,
        discovery: 'failed',
      }),
      expiresAt: 123_456,
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
      completedBytes: 12n,
      selectionFacts: Object.freeze({
        discoveredFileCount: 1n,
        discoveredBytes: 11n,
        discovery: 'complete',
      }),
      expiresAt: 123_456,
    })

    expect(() => canonicalReceiveLifecycleStateBytes(lifecycle)).toThrow(/do not contain completed output/)
  })

  it('round-trips Direct ZIP coordinates without reinterpreting selected payload as archive bytes', async () => {
    const lifecycle: ReceiveLifecycleState = Object.freeze({
      ...base(),
      kind: 'resumable-receive',
      payloadKind: 'direct-zip',
      directZipCheckpointDigest: identity(32, 3),
      safeSelectedPayloadBytes: 91n,
      committedArchiveLength: 144n,
      checkpointPhase: 'inside-member',
      expiresAt: 123_456,
    })
    const bytes = canonicalReceiveLifecycleStateBytes(lifecycle)
    expect(decodeReceiveLifecycleState(bytes)).toEqual(lifecycle)
    expect(decodeStoredReceiveLifecycleState(await storedReceiveLifecycleState(lifecycle)))
      .toEqual(lifecycle)
  })

  it('appends recovery gates as stable nonterminal states in frozen byte order', async () => {
    const expected = [
      ['authorization-required', RECEIVE_STATE_AUTHORIZATION_REQUIRED],
      ['target-verification-required', RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED],
      ['destination-space-required', RECEIVE_STATE_DESTINATION_SPACE_REQUIRED],
    ] as const
    for (const [kind, byte] of expected) {
      const gate = await createDirectZipRecoveryGateV1({
        operationId: base().operationId,
        receiveIntentDigest: base().receiveIntentDigest,
        kind,
        checkpointDigest: identity(32, 4),
        ...(kind === 'destination-space-required'
          ? { additionalTemporaryBytesUpperBound: 4096n }
          : {}),
      })
      const lifecycle: ReceiveLifecycleState = Object.freeze({
        ...base(),
        kind,
        recoveryGateDigest: gate.digest,
        expiresAt: 123_456,
      })
      expect(receiveStateByte(lifecycle)).toBe(byte)
      await expect(decodeDirectZipRecoveryGateV1(gate.canonicalBytes)).resolves.toEqual(gate)
      expect(decodeReceiveLifecycleState(canonicalReceiveLifecycleStateBytes(lifecycle)))
        .toEqual(lifecycle)
    }
  })

  it('rejects V1 lifecycle bytes instead of guessing a payload interpretation', () => {
    const legacy = canonicalRecord('windshare/receive-lifecycle-state/v1', 1, [])
    expect(() => decodeReceiveLifecycleState(legacy)).toThrow()
  })

  it('binds a temporary-space upper bound only to the destination-space gate', async () => {
    await expect(createDirectZipRecoveryGateV1({
      operationId: base().operationId,
      receiveIntentDigest: base().receiveIntentDigest,
      kind: 'authorization-required',
      checkpointDigest: identity(32, 4),
      additionalTemporaryBytesUpperBound: 1n,
    })).rejects.toThrow('only a destination-space recovery gate')
    await expect(createDirectZipRecoveryGateV1({
      operationId: base().operationId,
      receiveIntentDigest: base().receiveIntentDigest,
      kind: 'destination-space-required',
      checkpointDigest: identity(32, 4),
    })).rejects.toThrow('only a destination-space recovery gate')
  })

  it('reduces Direct ZIP pause, recovery gates, resume, and target deletion without aliasing', () => {
    const context = {
      planKind: 'direct-resumable-zip' as const,
      preparationRequired: false,
      activeLeaseId: identity(16, 5),
      nowMilliseconds: 100,
    }
    const initial = initialReceiveLifecycleState({
      operationId: base().operationId,
      receiveIntentDigest: base().receiveIntentDigest,
    })
    const receiving = reduceReceiveLifecycle(initial, {
      kind: 'receive-started',
      expectedGeneration: initial.generation,
      leaseId: context.activeLeaseId,
    }, context).state
    const paused = reduceReceiveLifecycle(receiving, {
      kind: 'direct-zip-pause-verified',
      expectedGeneration: receiving.generation,
      leaseId: context.activeLeaseId,
      checkpointDigest: identity(32, 6),
      safeSelectedPayloadBytes: 7n,
      committedArchiveLength: 9n,
      checkpointPhase: 'between-members',
    }, context).state
    expect(paused).toMatchObject({
      kind: 'resumable-receive',
      payloadKind: 'direct-zip',
      safeSelectedPayloadBytes: 7n,
      committedArchiveLength: 9n,
    })
    const gated = reduceReceiveLifecycle(paused, {
      kind: 'direct-zip-recovery-gated',
      expectedGeneration: paused.generation,
      leaseId: context.activeLeaseId,
      gateKind: 'target-verification-required',
      recoveryGateDigest: identity(32, 7),
    }, context).state
    expect(gated.kind).toBe('target-verification-required')
    const resumed = reduceReceiveLifecycle(gated, {
      kind: 'direct-zip-recovery-resumed',
      expectedGeneration: gated.generation,
      leaseId: context.activeLeaseId,
    }, context).state
    expect(resumed).toMatchObject({ kind: 'receiving', activeLeaseId: context.activeLeaseId })
    const deleted = reduceReceiveLifecycle(resumed, {
      kind: 'restart-boundary-verified',
      expectedGeneration: resumed.generation,
      leaseId: context.activeLeaseId,
      reason: 'target-deleted',
      receiptDigest: identity(32, 8),
    }, context).state
    expect(deleted).toMatchObject({ kind: 'restart-required', reason: 'target-deleted' })
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
