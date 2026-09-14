import { describe, expect, it, vi } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import * as progressiveBackend from '../../src/output/origin-private/progressive-backend'
import type { OriginPrivateProgressiveZipBackend } from '../../src/output/origin-private/progressive-backend'
import type { AuthorityOwnedReceiveOperationContinuation } from '../../src/output/resume/reopen-authority'
import type { ReceiveOperationRepository, ReceiveOperationTransition } from '../../src/output/workspace/repository'
import { WorkspaceOperationStages } from '../../src/output/workspace/stages'
import { storedReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import { ReceiveLifecycleNotifications } from '../../src/output/workspace/lifecycle/observation'
import { WorkspaceReceiveOperation } from '../../src/ui/browser-receive/workspace-operation'
import type { BrowserReceiveWindow } from '../../src/ui/browser-receive/contracts'
import { withDurableLifecycleSettlementTimeout } from '../../src/transfer/settlement/v2-output'
import * as progressiveExecution from '../../src/transfer/settlement/progressive-workspace-execution'
import {
  catalogFixture, digestIdentity, identity, identityText, readerFixture,
  receiveIntentFixture, transferJobFixture,
} from '../transfer/v2-job-fixture'
import { deferred, manualSettlementDeadline } from '../transfer/settlement-deadline'

type Operation = Extract<AuthorityOwnedReceiveOperationContinuation, { kind: 'workspace-progressive-zip' }>['operation']

async function fixture() {
  const intent = await receiveIntentFixture({
    planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection: new V2SelectionPolicy(true),
  })
  let lifecycle: ReceiveLifecycleState = {
    kind: 'receiving', operationId: intent.operationId, receiveIntentDigest: intent.digest,
    generation: 4n, activeLeaseId: identityText(16),
  }
  const notifications = new ReceiveLifecycleNotifications()
  const repository = {
    subscribeLifecycle: notifications.subscribe.bind(notifications),
    readLifecycle: async () => storedReceiveLifecycleState(lifecycle),
    commitTransition: vi.fn(async (transition: ReceiveOperationTransition) => {
      expect(transition.expectedLifecycleGeneration).toBe(lifecycle.generation)
      if (transition.lifecycle !== undefined) lifecycle = transition.lifecycle
      notifications.publish(lifecycle)
    }),
  } as unknown as ReceiveOperationRepository
  const stages = await WorkspaceOperationStages.open({
    repository, receiveIntent: intent, leaseId: identityText(16),
    clock: () => 1_000, contentRequests: { count: () => 0n },
  })
  const discard = vi.spyOn(stages, 'discard').mockImplementation(async () => {
    throw new Error('Retained ZIP must never be discarded by failed admission')
  })
  const close = vi.fn(async () => undefined)
  const checkpoint = {
    object: { operationId: intent.operationId, objectId: digestIdentity(33), handleId: 'zip-handle', kind: 'zip-archive' },
    generation: 7n, physicalLength: 128n, discoveryComplete: false, artifactState: 'receiving', entryCount: 1n,
  }
  const store = {
    readCheckpoint: async () => checkpoint,
    readEntries: async ({ afterSequence }: { afterSequence?: bigint }) => afterSequence === undefined
      ? [{
          kind: 'file', revision: { exactSize: 64n },
          ranges: [{ start: 0n, end: 64n }], zipLayout: { sequence: 0n },
        }] : [],
  }
  const backend = {
    archive: { close, checkpoint: vi.fn(async () => checkpoint), state: checkpoint },
    store, object: checkpoint.object, close: vi.fn(async () => undefined),
  } as unknown as OriginPrivateProgressiveZipBackend
  const operation = {
    intent, lifecycle, repository, stages,
    progressiveContinuation: { backend, requirement: 'remote-content-needed' },
    admittedContent: { claim: {} }, close: vi.fn(async () => undefined),
  } as unknown as Operation
  return { operation, close, discard, checkpoint, lifecycle: () => lifecycle }
}

describe('reopened ZIP admission ownership', () => {
  it('retains the owned ZIP checkpoint when Pause arrives before execution reaches the job', async () => {
    const f = await fixture()
    const runtime = await WorkspaceReceiveOperation.reopenProgressive({
      windowPort: {} as BrowserReceiveWindow, operation: f.operation,
    })
    const catalog = catalogFixture([{ id: identity(2), entries: [] }])
    const readers = readerFixture([])
    const controller = new AbortController()
    const createExecution = progressiveExecution.createProgressiveWorkspaceExecution
    const create = vi.spyOn(progressiveExecution, 'createProgressiveWorkspaceExecution')
      .mockImplementationOnce(async input => {
        const execution = await createExecution(input)
        runtime.interrupt('pause', controller)
        return execution
      })
    try {
      const result = await transferJobFixture({
        catalog: catalog.catalog, selection: new V2SelectionPolicy(true),
        intent: runtime.intent, plans: runtime.plans,
        revisions: readers.revisions, broker: readers.broker,
      }).run(controller.signal)

      expect(result.worker.status).toBe('Paused')
      expect(result.lifecycle).toMatchObject({
        kind: 'resumable-receive', payloadKind: 'opfs-zip',
        completedBytes: 64n, checkpointGeneration: 7n,
      })
      expect(f.lifecycle()).toEqual(result.lifecycle)
      expect(f.close).toHaveBeenCalledOnce()
      expect(f.discard).not.toHaveBeenCalled()
      expect(catalog.loads).toEqual([])
      expect(readers.revisionRequests).toEqual([])
      expect(readers.blockRequests).toEqual([])
    } finally {
      create.mockRestore()
    }
  })

  it('starts a new recovery admission after an already admitted attempt is paused in the same tab', async () => {
    const f = await fixture()
    const runtime = await WorkspaceReceiveOperation.reopenProgressive({
      windowPort: {} as BrowserReceiveWindow, operation: f.operation,
    })
    const intent = runtime.intent
    if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'zip-archive') {
      throw new Error('Expected a workspace ZIP intent')
    }
    await runtime.plans.openWorkspaceZip(
      { ...intent, plan: intent.plan, artifact: intent.artifact }, new AbortController().signal,
    )
    const backend = f.operation.progressiveContinuation.backend
    const paused = await f.operation.stages.progressive.pause(backend.store, backend.archive.state)
    const open = vi.spyOn(progressiveBackend, 'openOriginPrivateProgressiveZipBackend').mockResolvedValueOnce(backend)
    try {
      expect(await runtime.startLifecycleAction('continue', paused)).toMatchObject({ resumeTransfer: true })
      const settled = await runtime.settleTransferAdmissionFailure(new Error('resume connection failed'))
      expect(settled.lifecycle).toMatchObject({ kind: 'resumable-receive', payloadKind: 'opfs-zip', completedBytes: 64n })
      expect(f.discard).not.toHaveBeenCalled()
    } finally {
      open.mockRestore()
    }
  })

  it('restores real checkpoint progress through the production runtime before execution starts', async () => {
    const f = await fixture()
    const runtime = await WorkspaceReceiveOperation.reopenProgressive({
      windowPort: {} as BrowserReceiveWindow, operation: f.operation,
    })
    const [first, second] = await Promise.all([
      runtime.settleTransferAdmissionFailure(new Error('connection unavailable')),
      runtime.settleTransferAdmissionFailure(new Error('activation also failed')),
    ])
    expect(first).toEqual(second)
    expect(first.lifecycle).toMatchObject({
      kind: 'resumable-receive', payloadKind: 'opfs-zip', completedBytes: 64n,
      completedFileCount: 1n, occupiedBytes: 128n, checkpointGeneration: 7n,
    })
    expect(first.workspaceUsage).toEqual({ ownedBytes: 128n })
    expect(f.lifecycle()).toEqual(first.lifecycle)
    expect(f.close).toHaveBeenCalledOnce()
    expect(f.discard).not.toHaveBeenCalled()
  })

  it('keeps the verified recovery result when writer closure crosses the admission deadline', async () => {
    const f = await fixture()
    const entered = deferred()
    const release = deferred()
    f.close.mockImplementationOnce(async () => { entered.resolve(); await release.promise })
    const runtime = await WorkspaceReceiveOperation.reopenProgressive({
      windowPort: {} as BrowserReceiveWindow, operation: f.operation,
    })
    const deadline = manualSettlementDeadline()
    const running = withDurableLifecycleSettlementTimeout('restore ZIP admission', 1, signal =>
      runtime.plans.settleExecutionAdmissionFailure(runtime.intent, new Error('disconnected'), signal), deadline)
    let exposed = false
    const observation = running.then(() => { exposed = true })
    await entered.promise
    deadline.expire()
    await Promise.resolve()
    expect(exposed).toBe(false)
    expect(f.lifecycle().kind).toBe('receiving')
    release.resolve()
    expect(await running).toMatchObject({ kind: 'resumable-receive', payloadKind: 'opfs-zip', completedBytes: 64n })
    await observation
    expect(f.discard).not.toHaveBeenCalled()
  })
})
