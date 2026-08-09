import { describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  ReceiveOperationResumeAuthority,
  type ReceiveOperationMutationPort,
} from '../../src/output/resume/authority'
import {
  STABLE_RETENTION_MILLISECONDS,
  type ReceiveLifecycleState,
} from '../../src/output/workspace/state'

describe('receive operation resume authority', () => {
  it('offers only v6 lifecycle projections and consumes each reference once', async () => {
    const lifecycle = resumableReceive(10_000)
    const resume = vi.fn(async () => 'resumed')
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ resume }),
      clock: { now: () => 10_001 },
    })

    const inventory = await authority.listResumeState()
    expect(inventory.operations).toHaveLength(1)
    expect(inventory.operations[0]!.descriptor).toEqual(expect.objectContaining({
      operationId: lifecycle.operationId,
      lifecycleGeneration: 4n,
      continuation: 'resume-receive',
    }))
    expect(inventory.operations[0]!.descriptor.lifecycle)
      .not.toHaveProperty('verifiedRanges')

    await expect(authority.resume(inventory.operations[0]!)).resolves.toBe('resumed')
    await expect(authority.resume(inventory.operations[0]!))
      .rejects.toThrow('another authority')
    expect(resume).toHaveBeenCalledTimes(1)
  })

  it('routes an elapsed 24-hour deadline to the output expiry owner', async () => {
    const enteredAt = 5_000
    const lifecycle = resumableReceive(enteredAt)
    const resume = vi.fn(async () => 'must-not-run')
    const expire = vi.fn(async () => 'expired')
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ resume, expire }),
      clock: { now: () => enteredAt + STABLE_RETENTION_MILLISECONDS },
    })

    const inventory = await authority.listResumeState()
    const reference = inventory.operations[0]!
    expect(reference.descriptor.continuation).toBe('cleanup-expired')
    await expect(authority.resume(reference)).resolves.toBe('expired')
    expect(resume).not.toHaveBeenCalled()
    expect(expire).toHaveBeenCalledOnce()
  })

  it('closes inventory authority and reports cleanup uncertainty without partial export', async () => {
    const lifecycle = resumableReceive(10_000)
    const discard = vi.fn(async () => ({
      kind: 'needs-attention' as const,
      reason: 'cleanup-unknown' as const,
    }))
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ discard }),
      clock: { now: () => 10_001 },
    })

    const inventory = await authority.listResumeState()
    const reference = inventory.operations[0]!
    inventory.close()
    await expect(authority.discard(reference)).rejects.toThrow('closed inventory')
    expect(discard).not.toHaveBeenCalled()

    const second = await authority.listResumeState()
    await expect(authority.discard(second.operations[0]!)).resolves.toEqual({
      kind: 'needs-attention',
      reason: 'cleanup-unknown',
    })
    expect(await authority.discard(second.operations[0]!).catch((error) => error))
      .toBeInstanceOf(DOMException)
  })
})

function mutations(overrides: Partial<ReceiveOperationMutationPort<string>> = {}):
ReceiveOperationMutationPort<string> {
  return {
    resume: overrides.resume ?? (async () => 'resumed'),
    expire: overrides.expire ?? (async () => 'expired'),
    discard: overrides.discard ?? (async () => ({
      kind: 'discarded',
      cleanupReceiptDigest: identity(32, 9),
    })),
  }
}

function resumableReceive(enteredAt: number): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'resumable-receive',
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    generation: 4n,
    checkpointSetDigest: identity(32, 3),
    completedFileCount: 2n,
    completedBytes: 12n,
    expiresAt: enteredAt + STABLE_RETENTION_MILLISECONDS,
  })
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
