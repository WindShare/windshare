import { afterEach, describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import type { OutputFailureSinks } from '../../src/output/diagnostics'
import type { RecoverySummary } from '../../src/output/file-system-access/recovery-summary'
import {
  ReceiveOperationResumeAuthority,
  type ReceiveOperationMutationPort,
} from '../../src/output/resume/authority'
import {
  type ReceiveLifecycleState,
} from '../../src/output/workspace/state'

afterEach(() => vi.useRealTimers())

describe('receive operation resume authority', () => {
  it('keeps obsolete repair records cleanup-only after a long interruption', async () => {
    const lifecycle = resumableReceive()
    const readRecoverySummary = vi.fn(async () => { throw new Error('old repair must not decode') })
    const resume = vi.fn(async () => 'resumed')
    const cleanup = vi.fn(async () => 'cleaned')
    const catchUp = vi.fn(async () => 'caught-up')
    const discard = vi.fn(async () => ({ kind: 'record-forgotten' as const }))
    const authority = new ReceiveOperationResumeAuthority({
      source: {
        listLifecycleStates: async () => [lifecycle],
        isCleanupOnly: async () => true,
        readRecoverySummary,
      },
      mutations: { ...mutations({ resume, cleanup, discard }), catchUp },
    })
    const reference = async () => (await authority.listResumeState()).operations[0]!
    expect((await reference()).descriptor.continuation).toBe('cleanup-incompatible')
    await expect(authority.resume(await reference())).rejects.toThrow('only be forgotten')
    await expect(authority.catchUp(await reference())).rejects.toThrow('catch-up authority')
    await expect(authority.cleanup(await reference())).rejects.toThrow('cleanup authority')
    await expect(authority.discard(await reference())).resolves.toEqual({ kind: 'record-forgotten' })
    expect(readRecoverySummary).not.toHaveBeenCalled()
    expect(resume).not.toHaveBeenCalled()
    expect(cleanup).not.toHaveBeenCalled()
    expect(catchUp).not.toHaveBeenCalled()
    expect(discard).toHaveBeenCalledOnce()
  })

  it('offers only v6 lifecycle projections and consumes each reference once', async () => {
    const lifecycle = resumableReceive()
    const resume = vi.fn(async () => 'resumed')
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ resume }),
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

    await expect(authority.resume(inventory.operations[0]!, {
      retainedFileRecovery: 'preserve',
    })).rejects.toThrow('requires a validated recovery summary')
    await expect(authority.resume(inventory.operations[0]!)).resolves.toBe('resumed')
    await expect(authority.resume(inventory.operations[0]!))
      .rejects.toThrow('another authority')
    expect(resume).toHaveBeenCalledTimes(1)
  })

  it('binds a validated recovery summary and the selected retained-file action to one resume reference', async () => {
    const lifecycle = resumableReceive()
    const summary = recoverySummary(lifecycle)
    const resume = vi.fn(async () => 'resumed')
    const authority = new ReceiveOperationResumeAuthority({
      source: {
        listLifecycleStates: async () => [lifecycle],
        readRecoverySummary: async () => summary,
      },
      mutations: mutations({ resume }),
    })
    const inventory = await authority.listResumeState()
    const reference = inventory.operations[0]!

    expect(reference.recoverySummary).toBe(summary)
    await expect(authority.resume(reference)).rejects.toThrow(
      'requires a retained-file recovery choice',
    )
    await authority.resume(reference, { retainedFileRecovery: 'restart-owned-file' })
    expect(resume).toHaveBeenCalledWith(
      reference.descriptor,
      { retainedFileRecovery: 'restart-owned-file' },
    )
  })

  it('preserves unfinished receives after a year without expiring', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2030-01-01'))
    const lifecycle = resumableReceive()
    const resume = vi.fn(async () => 'resumed')
    const cleanup = vi.fn(async () => 'cleaned')
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ resume, cleanup }),
    })

    const inventory = await authority.listResumeState()
    const reference = inventory.operations[0]!
    expect(reference.descriptor.continuation).toBe('resume-receive')
    expect(reference.descriptor).not.toHaveProperty('expiresAt')
    await expect(authority.resume(reference)).resolves.toBe('resumed')
    expect(resume).toHaveBeenCalledOnce()
    expect(cleanup).not.toHaveBeenCalled()
  })

  it('forwards the exact attempt-local output capability through resume and discard', async () => {
    const lifecycle = resumableReceive()
    const resume = vi.fn(async () => 'resumed')
    const cleanup = vi.fn(async () => 'cleaned')
    const discard = vi.fn(async () => ({ kind: 'already-absent' as const }))
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ resume, cleanup, discard }),
    })
    const failures = Object.freeze({}) as OutputFailureSinks

    const resumeInventory = await authority.listResumeState()
    await authority.resume(resumeInventory.operations[0]!, { failures })
    expect(resume).toHaveBeenCalledWith(expect.any(Object), { failures })

    const discardInventory = await authority.listResumeState()
    await authority.discard(discardInventory.operations[0]!, failures)
    expect(discard).toHaveBeenCalledWith(expect.any(Object), failures)

  })

  it('consumes an explicit retained-cleanup reference exactly once', async () => {
    const lifecycle: ReceiveLifecycleState = Object.freeze({
      kind: 'published',
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
      generation: 5n,
      receiptDigest: identity(32, 3),
      cleanupState: 'cleanup-pending',
    })
    const cleanup = vi.fn(async () => 'cleaned')
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ cleanup }),
    })
    const inventory = await authority.listResumeState()
    const reference = inventory.operations[0]!

    await expect(authority.cleanup(reference)).resolves.toBe('cleaned')
    await expect(authority.cleanup(reference)).rejects.toThrow('another authority')
    expect(cleanup).toHaveBeenCalledOnce()
  })

  it('closes inventory authority and reports cleanup uncertainty without partial export', async () => {
    const lifecycle = resumableReceive()
    const discard = vi.fn(async () => ({
      kind: 'needs-attention' as const,
      reason: 'cleanup-unknown' as const,
    }))
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle] },
      mutations: mutations({ discard }),
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
    cleanup: overrides.cleanup ?? (async () => 'cleaned'),
    discard: overrides.discard ?? (async () => ({
      kind: 'discarded',
      cleanupReceiptDigest: identity(32, 9),
    })),
  }
}

function resumableReceive(): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    generation: 4n,
    checkpointSetDigest: identity(32, 3),
    completedFileCount: 2n,
    completedBytes: 12n,
    selectionFacts: Object.freeze({
      discoveredFileCount: 3n,
      discoveredBytes: 20n,
      discovery: 'failed',
    }),
  })
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}

function recoverySummary(lifecycle: ReceiveLifecycleState): RecoverySummary {
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set') {
    throw new TypeError('recovery summary fixture requires a resumable file set')
  }
  return Object.freeze({
    lifecycleGeneration: lifecycle.generation,
    checkpointSetDigest: lifecycle.checkpointSetDigest,
    discoveredFileCount: 3n,
    discoveredBytes: 20n,
    discovery: 'known-so-far',
    completedFileCount: lifecycle.completedFileCount,
    completedBytes: lifecycle.completedBytes,
    incompleteFileCount: 1n,
    verifiedPartialFileCount: 1n,
    verifiedPartialBytes: 4n,
    unstartedFileCount: 1n,
    unstartedBytes: 4n,
    preservingRemainingBytes: 4n,
    restartRemainingBytes: 8n,
    restartRedownloadBytes: 4n,
    maximumPreservingTemporaryBytes: 4n,
  })
}