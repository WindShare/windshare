import { beforeEach, describe, expect, it, vi } from 'vitest'
import { deliveryPolicy } from '../output/browser-delivery-fixture'
import { capableWindow, directoryHandle } from './v2-browser-receive-composition-fixture'
import { deferred } from './fsa-route-activation-fixture'

const mocks = vi.hoisted(() => ({
  operation: {} as Record<string, unknown>, lifecycle: {} as Record<string, unknown>, policy: {} as Record<string, unknown>,
  openTarget: vi.fn(), openDelivery: vi.fn(), openCleanup: vi.fn(), cleanup: vi.fn(), discard: vi.fn(), discardIncomplete: vi.fn(),
  close: vi.fn(), release: vi.fn(), closeRepository: vi.fn(),
  begin: vi.fn(), reconcile: vi.fn(), save: vi.fn(), drain: vi.fn(), rootRelease: vi.fn(),
}))
vi.mock('../../src/output/browser/indexeddb-repository', () => ({
  IndexedDbReceiveOperationRepository: { open: async () => ({
    readRecord: async () => ({}), readLifecycle: async () => ({}), close: mocks.closeRepository,
  }) },
}))
vi.mock('../../src/output/browser/session-lease', () => ({
  acquireBrowserReceiveOperationLease: async () => ({ operationId: mocks.policy.operationId, leaseId: 'local-lease', release: mocks.release }),
}))
vi.mock('../../src/output/browser-delivery/indexeddb', () => ({
  IndexedDbBrowserDeliveryRepository: { open: async () => ({ readPolicy: async () => mocks.policy, close: () => undefined }) },
}))
vi.mock('../../src/output/browser-delivery/assembly', () => ({
  openBrowserFolderDelivery: mocks.openDelivery, openBrowserFolderDeliveryCleanup: mocks.openCleanup,
}))
vi.mock('../../src/output/file-system-access/session', () => ({ reopenFileSystemAccessOutput: mocks.openTarget }))
vi.mock('../../src/output/browser-delivery/recovery/local-lifecycle', () => ({
  beginBrowserDeliveryLocalMutation: mocks.begin, reconcileBrowserDeliveryLifecycle: mocks.reconcile,
}))
vi.mock('../../src/output/workspace/records', async importOriginal => ({
  ...await importOriginal<Record<string, unknown>>(), decodeStoredReceiveOperation: async () => mocks.operation,
}))
vi.mock('../../src/output/workspace/state-codec', async importOriginal => ({
  ...await importOriginal<Record<string, unknown>>(), decodeStoredReceiveLifecycleState: () => mocks.lifecycle,
}))

import { runRetainedBrowserFolderAction } from '../../src/ui/browser-receive/fsa/local-delivery'

beforeEach(() => {
  vi.clearAllMocks()
  mocks.policy = { ...deliveryPolicy() }
  mocks.operation = { receiveIntentDigest: mocks.policy.receiveIntentDigest,
    receiveIntent: { operationId: mocks.policy.operationId, digest: mocks.policy.receiveIntentDigest, plan: { kind: 'direct-tree' } } }
  mocks.lifecycle = { generation: 2n, operationId: mocks.policy.operationId, receiveIntentDigest: mocks.policy.receiveIntentDigest,
    kind: 'partial-directory', reason: 'stopped' }
  mocks.openTarget.mockRejectedValue(new DOMException('Destination is unavailable', 'NotAllowedError'))
  mocks.openCleanup.mockResolvedValue({ cleanupStaging: mocks.cleanup, discardStaging: mocks.discard,
    discardIncompleteStaging: mocks.discardIncomplete, close: mocks.close })
  mocks.cleanup.mockResolvedValue(undefined)
  mocks.discard.mockResolvedValue(undefined)
  mocks.discardIncomplete.mockResolvedValue(undefined)
  mocks.begin.mockResolvedValue(mocks.lifecycle)
  mocks.reconcile.mockResolvedValue(mocks.lifecycle)
})

describe('local staging cleanup without destination access', () => {
  it.each(['cleanup-staging', 'discard-staging', 'discard-incomplete-staging'] as const)('runs %s with only retained storage authority', async action => {
    const reference = { operationId: String(mocks.policy.operationId), receiveIntentDigest: String(mocks.policy.receiveIntentDigest), lifecycleGeneration: 2n }
    const picker = vi.fn(async () => directoryHandle())
    await runRetainedBrowserFolderAction(capableWindow(picker), reference, action, new AbortController().signal)
    expect(mocks.openCleanup).toHaveBeenCalledOnce()
    expect(mocks.openTarget).not.toHaveBeenCalled()
    expect(mocks.openDelivery).not.toHaveBeenCalled()
    expect(picker).not.toHaveBeenCalled()
    const mutations = { 'cleanup-staging': mocks.cleanup, 'discard-incomplete-staging': mocks.discardIncomplete, 'discard-staging': mocks.discard }
    expect(mutations[action]).toHaveBeenCalledOnce()
    expect(mocks.close).toHaveBeenCalledOnce()
    expect(mocks.release).toHaveBeenCalledOnce()
    expect(mocks.closeRepository).toHaveBeenCalledOnce()
    expect(mocks.begin).not.toHaveBeenCalled()
    expect(mocks.reconcile).toHaveBeenCalledOnce()
    expect(mocks.close.mock.invocationCallOrder[0]).toBeLessThan(mocks.reconcile.mock.invocationCallOrder[0]!)
    expect(mocks.reconcile.mock.invocationCallOrder[0]).toBeLessThan(mocks.release.mock.invocationCallOrder[0]!)
  })

  it('rejects abandonment after the authoritative lifecycle is paused or has advanced', async () => {
    const reference = { operationId: String(mocks.policy.operationId), receiveIntentDigest: String(mocks.policy.receiveIntentDigest), lifecycleGeneration: 2n }
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    mocks.lifecycle = { ...mocks.lifecycle, kind: 'resumable-receive', payloadKind: 'file-set' }
    await expect(runRetainedBrowserFolderAction(windowPort, reference, 'discard-incomplete-staging', new AbortController().signal))
      .rejects.toMatchObject({ name: 'InvalidStateError' })
    mocks.lifecycle = { ...mocks.lifecycle, kind: 'partial-directory', reason: 'stopped', generation: 3n }
    await expect(runRetainedBrowserFolderAction(windowPort, reference, 'discard-incomplete-staging', new AbortController().signal))
      .rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(mocks.openCleanup).not.toHaveBeenCalled()
    expect(mocks.discardIncomplete).not.toHaveBeenCalled()
    expect(mocks.reconcile).not.toHaveBeenCalled()
    expect(mocks.release).toHaveBeenCalledTimes(2)
  })

  it('holds operation authority until an incomplete-stage deletion finishes', async () => {
    const started = deferred<void>()
    const removed = deferred<void>()
    mocks.discardIncomplete.mockImplementation(async () => { started.resolve(undefined); await removed.promise })
    const pending = runRetainedBrowserFolderAction(capableWindow(vi.fn(async () => directoryHandle())), {
      operationId: String(mocks.policy.operationId), receiveIntentDigest: String(mocks.policy.receiveIntentDigest), lifecycleGeneration: 2n,
    }, 'discard-incomplete-staging', new AbortController().signal)
    await started.promise
    expect(mocks.release).not.toHaveBeenCalled()
    expect(mocks.closeRepository).not.toHaveBeenCalled()
    removed.resolve(undefined)
    await pending
    expect(mocks.release).toHaveBeenCalledOnce()
  })

  it('releases operation authority while preserving a failed stage deletion for retry', async () => {
    const failure = new Error('Staging deletion failed')
    mocks.cleanup.mockRejectedValue(failure)
    await expect(runRetainedBrowserFolderAction(capableWindow(vi.fn(async () => directoryHandle())), {
      operationId: String(mocks.policy.operationId), receiveIntentDigest: String(mocks.policy.receiveIntentDigest), lifecycleGeneration: 2n,
    }, 'cleanup-staging', new AbortController().signal)).rejects.toBe(failure)
    expect(mocks.close).toHaveBeenCalledOnce()
    expect(mocks.release).toHaveBeenCalledOnce()
    expect(mocks.openTarget).not.toHaveBeenCalled()
  })

  it('records the local mutation before opening output and reconciles after failed copies drain', async () => {
    const failure = new Error('One received file could not be copied')
    mocks.openTarget.mockResolvedValue({ activate: async () => undefined,
      closeForTerminalSettlement: mocks.drain, releaseRootLease: mocks.rootRelease })
    mocks.save.mockRejectedValue(failure)
    mocks.openDelivery.mockResolvedValue({ saveStagedFiles: mocks.save, close: mocks.close })
    await expect(runRetainedBrowserFolderAction(capableWindow(vi.fn(async () => directoryHandle())), {
      operationId: String(mocks.policy.operationId), receiveIntentDigest: String(mocks.policy.receiveIntentDigest), lifecycleGeneration: 2n,
    }, 'save-staged-files', new AbortController().signal)).rejects.toBe(failure)
    expect(mocks.begin.mock.invocationCallOrder[0]).toBeLessThan(mocks.openTarget.mock.invocationCallOrder[0]!)
    expect(mocks.close.mock.invocationCallOrder[0]).toBeLessThan(mocks.reconcile.mock.invocationCallOrder[0]!)
    expect(mocks.drain.mock.invocationCallOrder[0]).toBeLessThan(mocks.reconcile.mock.invocationCallOrder[0]!)
    expect(mocks.reconcile.mock.invocationCallOrder[0]).toBeLessThan(mocks.release.mock.invocationCallOrder[0]!)
    expect(mocks.rootRelease).toHaveBeenCalledOnce()
  })
})
