import { describe, expect, it, vi } from 'vitest'
import { createBrowserReceiveComposition } from '../../src/ui/v2-browser-receive-composition'
import type { BrowserDeliveryRepository } from '../../src/output/browser-delivery/repository'
import type { BrowserDeliveryRecordV1 } from '../../src/output/browser-delivery/model'
import { deliveryFixture } from '../output/browser-delivery-fixture'
import { capableWindow, directoryHandle, FakeResumeSource } from './v2-browser-receive-composition-fixture'

function retainedFixture() {
  const fixture = deliveryFixture()
  const complete = fixture.advance(fixture.initial, { kind: 'staged-complete', stage: fixture.stage! })
  const source = new FakeResumeSource([{
    operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 1n,
    kind: 'resumable-receive', payloadKind: 'file-set', checkpointSetDigest: fixture.policy.digest,
    completedFileCount: 0n, completedBytes: 0n,
    selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 8n, discovery: 'failed' },
  }])
  const repository: BrowserDeliveryRepository = {
    installPolicy: async () => fixture.policy,
    readPolicy: async () => fixture.policy,
    readFile: async () => complete,
    createFile: async () => complete,
    replaceFile: async () => undefined,
    finalizeDirect: async () => { throw new Error('Read-only retained inventory cannot finalize output') },
    authorizeRestart: async () => { throw new Error('Read-only retained inventory cannot restart output') },
    scanFiles: async () => ({ records: [complete] as readonly BrowserDeliveryRecordV1[] }),
    close: vi.fn(),
  }
  return { fixture, source, repository }
}

describe('retained folder local action authority', () => {
  it('can save complete staging without a sender continuation provider or another picker', async () => {
    const { source, repository } = retainedFixture()
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
