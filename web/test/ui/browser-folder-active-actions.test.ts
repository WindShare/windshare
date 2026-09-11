import { describe, expect, it, vi } from 'vitest'
import { FSAReceiveOperation } from '../../src/ui/browser-receive/fsa'
import { FSAResourceOwner } from '../../src/ui/browser-receive/fsa-resource-owner'
import { BrowserFolderDeliveryProgress, type BrowserFolderDeliveryContext } from '../../src/ui/browser-receive/fsa/folder-delivery'
import { summarizeBrowserDeliveries } from '../../src/output/browser-delivery/retained'
import { storedReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import { deliveryFixture, deliveryIdentity } from '../output/browser-delivery-fixture'
import { deferred } from './fsa-route-activation-fixture'

const targets = vi.hoisted(() => ({ reopen: vi.fn(), begin: vi.fn(), reconcile: vi.fn() }))
vi.mock('../../src/output/file-system-access/session', async importOriginal => ({
  ...await importOriginal<Record<string, unknown>>(), reopenFileSystemAccessOutput: targets.reopen,
}))
vi.mock('../../src/output/browser-delivery/recovery/local-lifecycle', () => ({
  beginBrowserDeliveryLocalMutation: targets.begin, reconcileBrowserDeliveryLifecycle: targets.reconcile,
}))
// These cases exercise the bound runtime after source settlement; no receive job is opened.
vi.mock('../../src/transfer/settlement/v2-plan-authority', () => ({ createV2PlanExecutionAuthority: async () => ({}) }))

async function activeFixture() {
  vi.clearAllMocks()
  const fixture = deliveryFixture()
  const lifecycle: ReceiveLifecycleState = {
    operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 2n,
    kind: 'partial-directory', reason: 'stopped', successCount: 1n, failureCount: 1n, receiptDigest: fixture.policy.digest,
  }
  let current: ReceiveLifecycleState = lifecycle
  const lease = { operationId: lifecycle.operationId, leaseId: deliveryIdentity(30, 16), acquiredAt: 100 }
  const progress = new BrowserFolderDeliveryProgress()
  const initialClose = vi.fn(async () => undefined)
  const cleanupClose = vi.fn(async () => undefined)
  const cleanup = vi.fn(async () => undefined)
  const abandon = vi.fn(async () => undefined)
  const save = vi.fn(async () => undefined)
  const initial = {
    getSummary: () => summarizeBrowserDeliveries(fixture.policy, [fixture.initial]),
    subscribe: () => () => undefined, close: initialClose, saveStagedFiles: save,
  }
  const openCleanup = vi.fn(async () => ({ ...initial, close: cleanupClose,
    cleanupStaging: cleanup, discardIncompleteStaging: abandon }))
  const context = { progress, preference: 'automatic', storage: {}, storageFacts: async () => ({}),
    open: async () => initial, openCleanup } as unknown as BrowserFolderDeliveryContext
  const readLease = vi.fn(async () => lease)
  const repository = { readLease, readLifecycle: async () => storedReceiveLifecycleState(current) }
  const input = {
    intent: { operationId: lifecycle.operationId, digest: lifecycle.receiveIntentDigest,
      artifact: { digest: deliveryIdentity(31) }, plan: { kind: 'direct-tree' } },
    lifecycle, repository, lease, session: {}, settlement: {},
    resources: new FSAResourceOwner({}), folderDelivery: context,
    transferJobId: deliveryIdentity(32, 16), outputSessionId: deliveryIdentity(33, 16),
    attemptIdentities: { createTransferJobId: () => deliveryIdentity(34, 16), createOutputSessionId: () => deliveryIdentity(35, 16) },
  } as unknown as Parameters<typeof FSAReceiveOperation.createCommitted>[0]
  const runtime = await FSAReceiveOperation.createCommitted(input)
  targets.begin.mockImplementation(async () => current)
  targets.reconcile.mockImplementation(async () => current)
  targets.reopen.mockResolvedValue({ activate: async () => undefined, close: async () => undefined })
  return { runtime, lifecycle, fixture, initialClose, cleanupClose, cleanup, abandon, save, openCleanup, readLease,
    setLifecycle: (state: ReceiveLifecycleState) => { current = state } }
}

describe('active terminal folder storage actions', () => {
  it.each(['cleanup-staging', 'discard-incomplete-staging'] as const)(
    'runs %s on the settled operation without source or destination authority', async action => {
      const f = await activeFixture()
      const started = deferred<void>()
      const finish = deferred<void>()
      const local = action === 'cleanup-staging' ? f.cleanup : f.abandon
      local.mockImplementation(async () => { started.resolve(undefined); await finish.promise })
      const pending = f.runtime.startLifecycleAction(action, f.lifecycle)
      await started.promise
      expect(f.initialClose).toHaveBeenCalledOnce()
      expect(f.cleanupClose).not.toHaveBeenCalled()
      expect(targets.reopen).not.toHaveBeenCalled()
      finish.resolve(undefined)
      await expect(pending).resolves.toMatchObject({ lifecycle: f.lifecycle, actionOutcome: { kind: 'completed' } })
      expect((await pending).lifecycle).toBe(f.lifecycle)
      expect(f.cleanupClose).toHaveBeenCalledOnce()
      expect(targets.begin).not.toHaveBeenCalled()
    },
  )

  it('returns the latest terminal authority after a failed local cleanup and leaves retry reachable', async () => {
    const f = await activeFixture()
    const latest = { ...f.lifecycle, generation: 3n }
    const failure = new Error('stage deletion failed')
    f.cleanup.mockImplementation(async () => { f.setLifecycle(latest); throw failure })
    const result = await f.runtime.startLifecycleAction('cleanup-staging', f.lifecycle)
    expect(result).toMatchObject({ lifecycle: latest, actionOutcome: { kind: 'failed', error: failure } })
    f.cleanup.mockResolvedValue(undefined)
    await expect(f.runtime.startLifecycleAction('cleanup-staging', result.lifecycle)).resolves.toMatchObject({ actionOutcome: { kind: 'completed' } })
    expect(f.openCleanup).toHaveBeenCalledTimes(2)
  })

  it('rejects stale lifecycle, foreign lease, and paused abandonment before opening storage', async () => {
    const f = await activeFixture()
    f.setLifecycle({ ...f.lifecycle, generation: 3n })
    await expect(f.runtime.startLifecycleAction('discard-incomplete-staging', f.lifecycle)).rejects.toMatchObject({ name: 'InvalidStateError' })
    f.setLifecycle(f.lifecycle)
    f.readLease.mockResolvedValue({ operationId: f.lifecycle.operationId, leaseId: deliveryIdentity(40, 16), acquiredAt: 100 })
    await expect(f.runtime.startLifecycleAction('cleanup-staging', f.lifecycle)).rejects.toMatchObject({ name: 'InvalidStateError' })
    const paused: ReceiveLifecycleState = { ...f.lifecycle, kind: 'resumable-receive', payloadKind: 'file-set',
      checkpointSetDigest: f.fixture.policy.digest, completedFileCount: 0n, completedBytes: 0n,
      selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 8n, discovery: 'failed' } }
    await expect(f.runtime.startLifecycleAction('discard-incomplete-staging', paused)).rejects.toMatchObject({ name: 'NotSupportedError' })
    expect(f.openCleanup).not.toHaveBeenCalled()
    expect(f.initialClose).not.toHaveBeenCalled()
  })

  it('saves complete child staging while preserving the terminal source receipt', async () => {
    const f = await activeFixture()
    const result = await f.runtime.startLifecycleAction('save-staged-files', f.lifecycle)
    expect(f.save).toHaveBeenCalledOnce()
    expect(result.lifecycle).toBe(f.lifecycle)
    expect(result.actionOutcome).toEqual({ kind: 'completed' })
    expect(result.resumeTransfer).toBeUndefined()
    expect(result.recoverySummary).toBeUndefined()
  })
})
