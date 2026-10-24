import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { TransferPauseRequestedError } from '../../src/transfer/output/session'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ReceiveRuntimeState } from '../../src/application/receive/state'
import { ActiveReceiveCoordinator } from '../../src/application/receive/active/coordinator'
import { RetainedInventoryCoordinator } from '../../src/application/receive/retained/coordinator'
import { BrowserReceiveRuntime } from '../../src/application/receive/runtime'
import { V2OutputPresentationController } from '../../src/ui/v2-output'
import {
  FakeBoundRuntime, FakeJoinedShare, FakeReceiveComposition, WORKSPACE_ENVIRONMENT,
  deferred, retainedWorkspaceIntent, stableLifecycle, turns, waitFor, waitingRetainedOperation,
} from '../ui/v2-receiver-orchestration-fixture'

const cleanups: Array<() => Promise<void>> = []
afterEach(async () => {
  for (const close of cleanups.splice(0)) await close()
})

async function fixture() {
  const state = new ReceiveRuntimeState()
  const joined = new FakeJoinedShare(true)
  const operation = new FakeBoundRuntime(await retainedWorkspaceIntent(joined))
  operation.interrupt = (_control, transfer) => {
    joined.transferRuns.at(-1)!.resolve(stableLifecycle(state.getSnapshot().lifecycle!, 'resumable-receive'))
    transfer.abort(new TransferPauseRequestedError())
  }
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const errors: unknown[] = []
  const changed = vi.fn()
  const inventoryChanged = vi.fn()
  const active = new ActiveReceiveCoordinator({
    state, ownsJoinedShare: source => source === joined,
    onProgress: () => undefined, onFailure: error => errors.push(error),
    onActionError: error => errors.push(error),
    onOwnershipReleased: () => runtime.ownershipReleased(),
  })
  const retained: RetainedInventoryCoordinator = new RetainedInventoryCoordinator({
    receive, isDisposed: () => false, currentJoinedShare: () => joined,
    continuationBlocked: () => active.active || runtime.pending,
    localFinalizationBlocked: () => active.active || runtime.pending,
    remoteContinuationUnavailable: () => runtime.remoteContinuationUnavailable(),
    adoptContinuation: input => runtime.adoptContinuation(input),
    ownsRuntime: source => active.ownsRuntime(source),
    publish: inventoryChanged, onActionError: error => errors.push(error),
  })
  const runtime: BrowserReceiveRuntime<FakeJoinedShare> = new BrowserReceiveRuntime({
    state, active, retained, activation: { pending: false, invalidate: () => undefined },
    joined: () => joined, disposed: () => false,
    selectionIsEmpty: () => joined.selection.intentSummary().empty,
    onChanged: changed, projection: { stop: () => undefined }, onTaskCleared: () => undefined,
    onTaskAdopted: () => undefined, onBrowsingReady: () => undefined,
    onActionError: error => errors.push(error),
  })
  runtime.observeConnection({ kind: 'connected' })
  const result = {
    state, joined, operation, active, retained, receive, runtime, errors, changed, inventoryChanged,
    start: () => {
      const adoption = active.prepareAdoption({ joined, runtime: operation, selection: joined.selection.snapshot() })
      state.adoptRetainedReceiveIntentAtomically(operation.intent, operation.lifecycle, adoption.commit,
        operation.initialWorkspaceUsage, operation.activeControls)
      adoption.start()
    },
    close: async () => {
      retained.close(new DOMException('Test complete', 'AbortError'))
      await active.reset(new DOMException('Test complete', 'AbortError'))
      state.close()
    },
  }
  cleanups.push(result.close)
  return result
}

describe('browser receive authority without presentation', () => {
  it('pauses and resumes without constructing a UI controller, despite failing observers', async () => {
    const f = await fixture()
    f.state.subscribe(() => { throw new Error('View unavailable') })
    f.start()
    f.runtime.performLifecycleAction('save')
    expect(f.operation.lifecycleActions).toHaveLength(0)
    f.runtime.performLifecycleAction('pause')
    expect(f.joined.transferRuns[0]!.signal?.aborted).toBe(true)
    await waitFor(() => f.state.getSnapshot().lifecycle?.kind === 'resumable-receive' &&
      f.state.getSnapshot().receiveInterruption === null)
    f.runtime.performLifecycleAction('continue')
    await waitFor(() => f.joined.transferRuns.length === 2)
    expect(f.operation.lifecycleActions.map(action => action.action)).toEqual(['continue'])
    expect(f.errors).toEqual([])
  })

  it('keeps admission independent of a detached, stale presentation snapshot', async () => {
    const f = await fixture()
    f.start()
    const view = new V2OutputPresentationController(f.state)
    const stale = view.getSnapshot()
    view.close()
    f.runtime.performLifecycleAction('pause')
    await waitFor(() => f.runtime.activeLifecycleActionAdmission('continue').allowed)
    expect(view.getSnapshot()).toBe(stale)
    expect(stale.lifecyclePresentation?.actions.map(action => action.kind)).not.toContain('continue')
    f.runtime.performLifecycleAction('continue')
    await waitFor(() => f.joined.transferRuns.length === 2)
    expect(f.errors).toEqual([])
  })

  it('keeps recovery blocked between a durable pause observation and execution settlement', async () => {
    const f = await fixture()
    f.start()
    const paused = stableLifecycle(f.operation.lifecycle, 'resumable-receive')
    f.operation.lifecycleNotifications.publish(paused)
    f.runtime.performLifecycleAction('continue')
    expect(f.operation.lifecycleActions).toHaveLength(0)
    expect(f.runtime.activeLifecycleActionAdmission('continue').allowed).toBe(false)
    f.joined.transferRuns[0]!.resolve(paused)
    await waitFor(() => f.runtime.activeLifecycleActionAdmission('continue').allowed)
    f.runtime.performLifecycleAction('continue')
    await waitFor(() => f.joined.transferRuns.length === 2)
  })

  it('starts saving in the original user-action stack and admits a pending command only once', async () => {
    const f = await fixture()
    f.start()
    f.joined.transferRuns[0]!.resolve(stableLifecycle(f.operation.lifecycle, 'waiting-to-save'))
    await waitFor(() => f.runtime.activeLifecycleActionAdmission('save').allowed)
    let inClick = true
    f.operation.lifecycleActionStack = () => inClick
    f.runtime.performLifecycleAction('save')
    inClick = false
    f.runtime.performLifecycleAction('save')
    expect(f.operation.lifecycleActions).toEqual([{ action: 'save', inClickStack: true }])
    await waitFor(() => f.state.getSnapshot().lifecycle?.kind === 'publishing-managed')
    expect(f.errors).toEqual([])
  })

  it('releases a delivered task only after both execution and output detachment settle', async () => {
    const f = await fixture()
    f.start()
    const detached = deferred<void>()
    const detach = vi.spyOn(f.operation, 'detach').mockImplementation(() => detached.promise)
    const delivered = stableLifecycle(f.operation.lifecycle, 'download-started')
    f.operation.lifecycleNotifications.publish(delivered)
    await turns()
    expect(detach).not.toHaveBeenCalled()
    expect(f.runtime.startBlockedReason()).not.toBeNull()
    f.joined.transferRuns[0]!.resolve(delivered)
    await waitFor(() => detach.mock.calls.length === 1)
    expect(f.active.active).toBe(true)
    expect(f.runtime.startBlockedReason()).not.toBeNull()
    detached.resolve()
    await waitFor(() => !f.active.active)
    expect(f.runtime.startBlockedReason()).toBeNull()
  })

  it('uses the inventory-owned token for offline local actions even when its observer fails', async () => {
    const f = await fixture()
    f.inventoryChanged.mockImplementation(() => { throw new Error('Downloads view unavailable') })
    f.receive.retainedOperations = [waitingRetainedOperation()]
    await f.retained.load()
    const operation = f.retained.getSnapshot().operations[0]!
    f.runtime.observeConnection({ kind: 'ended', reason: 'share-stopped' })
    expect(f.runtime.retainedActionAdmission(operation, 'save').allowed).toBe(true)
    expect(f.runtime.retainedActionAdmission({ ...operation }, 'save').allowed).toBe(false)
    f.runtime.performRetainedAction(operation, 'save')
    expect(f.receive.retainedActionCalls).toHaveLength(1)
    await waitFor(() => !f.retained.pending)
    expect(f.errors).toEqual([])
  })

  it('evaluates new-task admission from selection and connection facts', async () => {
    const f = await fixture()
    expect(f.runtime.startBlockedReason()).toBeNull()
    f.joined.replaceSelection(new V2SelectionPolicy(false))
    expect(f.runtime.startBlockedReason()).not.toBeNull()
    f.runtime.observeConnection({ kind: 'ended', reason: 'share-stopped' })
    expect(f.runtime.remoteContinuationUnavailable()).not.toBeNull()
    expect(f.runtime.startBlockedReason()).not.toBeNull()
  })
})
