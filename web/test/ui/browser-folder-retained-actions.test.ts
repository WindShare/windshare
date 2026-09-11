import { describe, expect, it, vi } from 'vitest'
import { createBrowserReceiveComposition } from '../../src/ui/v2-browser-receive-composition'
import type { BrowserDeliveryRepository } from '../../src/output/browser-delivery/repository'
import type { BrowserDeliveryRecordV1 } from '../../src/output/browser-delivery/model'
import { deliveryFixture } from '../output/browser-delivery-fixture'
import { capableWindow, directoryHandle, FakeResumeSource } from './v2-browser-receive-composition-fixture'
import { summarizeBrowserDeliveries } from '../../src/output/browser-delivery/retained'

// IndexedDB checkpoint reconciliation is covered by the browser storage contracts.
vi.mock('../../src/output/browser-delivery/retained-authority', () => ({
  readBrowserDeliveryResumeSummary: async ({ repository, operationId }: { repository: BrowserDeliveryRepository; operationId: string }) => {
    const policy = await repository.readPolicy(operationId)
    return policy === undefined ? undefined : summarizeBrowserDeliveries(policy, (await repository.scanFiles({ operationId })).records)
  },
}))

function retainedFixture(stopped = false, incomplete = false) {
  const fixture = deliveryFixture()
  let record = incomplete
    ? fixture.advance(fixture.initial, { kind: 'receiving', checkpoint: fixture.checkpoint('staged', 3n) })
    : fixture.advance(fixture.initial, { kind: 'staged-complete', stage: fixture.stage! })
  const source = new FakeResumeSource([{
    operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 1n,
    ...(stopped ? {
      kind: 'partial-directory' as const, reason: 'stopped' as const, successCount: 1n, failureCount: 1n, receiptDigest: fixture.policy.digest,
    } : { kind: 'resumable-receive' as const, payloadKind: 'file-set' as const, checkpointSetDigest: fixture.policy.digest,
    completedFileCount: 0n, completedBytes: 0n,
    selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 8n, discovery: 'failed' as const },
    }),
  }])
  const repository: BrowserDeliveryRepository = {
    installPolicy: async () => fixture.policy,
    readPolicy: async () => fixture.policy,
    readFile: async () => record,
    createFile: async () => record,
    replaceFile: async () => undefined,
    finalizeDirect: async () => { throw new Error('Read-only retained inventory cannot finalize output') },
    authorizeRestart: async () => { throw new Error('Read-only retained inventory cannot restart output') },
    scanFiles: async () => ({ records: [record] as readonly BrowserDeliveryRecordV1[] }),
    close: vi.fn(),
  }
  return { fixture, source, repository, update: (state: BrowserDeliveryRecordV1['state']) => { record = fixture.advance(record, state) } }
}

describe('retained folder local action authority', () => {
  it.each([false, true])('can save complete staging without a sender or another picker (stopped: %s)', async stopped => {
    const { source, repository } = retainedFixture(stopped)
    const picker = vi.fn(async () => directoryHandle())
    const local = vi.fn(async () => undefined)
    const composition = createBrowserReceiveComposition(capableWindow(picker), {
      openResumeSource: async () => source,
      openBrowserDeliveryRepository: async () => repository,
      runBrowserFolderLocalAction: local,
    })
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]!
    expect(operation.actions).toContain('save-staged-files')
    expect(operation.browserDelivery).toMatchObject({ stagedCompleteFiles: 1, targetSavedBytes: 0n, recoverableBytes: 8n })
    await expect(inventory.act({ ...operation }, 'save-staged-files', new AbortController().signal))
      .rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(local).not.toHaveBeenCalled()
    await expect(inventory.act(operation, 'save-staged-files', new AbortController().signal))
      .resolves.toEqual({ kind: 'completed' })
    expect(local).toHaveBeenCalledOnce()
    expect(local).toHaveBeenCalledWith(expect.anything(), operation, 'save-staged-files', expect.any(AbortSignal), undefined)
    expect(picker).not.toHaveBeenCalled()
    inventory.close()
    expect(repository.close).toHaveBeenCalledOnce()
  })

  it('reopens stopped incomplete abandonment as cleanup retry after durable deletion failure', async () => {
    const { fixture, source, repository, update } = retainedFixture(true, true)
    const picker = vi.fn(async () => directoryHandle())
    const local = vi.fn(async (_window: unknown, _reference: unknown, action: string) => {
      if (action === 'discard-incomplete-staging') {
        update({ kind: 'discarding', checkpoint: fixture.checkpoint('staged', 3n) })
        throw new Error('Staging deletion failed')
      }
      expect(action).toBe('cleanup-staging')
      update({ kind: 'discarded' })
    })
    const composition = createBrowserReceiveComposition(capableWindow(picker), {
      openResumeSource: async () => source, openBrowserDeliveryRepository: async () => repository,
      runBrowserFolderLocalAction: local,
    })
    const first = await composition.retained.list(new AbortController().signal)
    const stopped = first.operations[0]!
    expect(stopped.browserDelivery).toMatchObject({ incompleteStagedFiles: 1, stagedBytes: 3n, reservedStagingBytes: 8n })
    expect(stopped.actions).toEqual(['discard-incomplete-staging'])
    await expect(first.act(stopped, 'discard-incomplete-staging', new AbortController().signal)).rejects.toThrow('Staging deletion failed')
    first.close()
    const retry = await composition.retained.list(new AbortController().signal)
    expect(retry.operations[0]!.actions).toEqual(['cleanup-staging'])
    expect(retry.operations[0]!.lifecycle).toEqual(stopped.lifecycle)
    await retry.act(retry.operations[0]!, 'cleanup-staging', new AbortController().signal)
    retry.close()
    const clean = await composition.retained.list(new AbortController().signal)
    expect(clean.operations[0]!.actions).toEqual([])
    expect(clean.operations[0]!.browserDelivery).toMatchObject({ stagedBytes: 0n, reservedStagingBytes: 0n })
    expect(clean.operations[0]!.lifecycle).toEqual(stopped.lifecycle)
    expect(picker).not.toHaveBeenCalled()
    clean.close()
  })

  it('keeps target operation metadata until explicitly abandoned staging was removed', async () => {
    const { source, repository } = retainedFixture()
    const discard = vi.fn(async () => ({ kind: 'already-absent' as const }))
    const local = vi.fn(async () => { throw new Error('stage deletion failed') })
    const composition = createBrowserReceiveComposition(capableWindow(vi.fn(async () => directoryHandle())), {
      openResumeSource: async () => source,
      openBrowserDeliveryRepository: async () => repository,
      runBrowserFolderLocalAction: local,
      resumeMutations: {
        resume: async () => { throw new Error('unexpected remote resume') },
        cleanup: async () => { throw new Error('unexpected generic cleanup') },
        discard,
      },
    })
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]!
    await expect(inventory.act(operation, 'discard', new AbortController().signal)).rejects.toThrow('stage deletion failed')
    expect(local).toHaveBeenCalledWith(expect.anything(), operation, 'discard-staging', expect.any(AbortSignal), undefined)
    expect(discard).not.toHaveBeenCalled()
    inventory.close()
  })
})
