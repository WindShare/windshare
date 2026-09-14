import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReceiveLifecycleListener } from '../../src/output/workspace/lifecycle/observation'
import { composeTasks } from '../../src/ui/experience/task-composition'
import {
  FakeBoundRuntime, FakeJoinedShare, FakeReceiveComposition, WORKSPACE_ENVIRONMENT,
  controllerFor, identityText, next, resetOrchestrationTestEnvironment,
  stableLifecycle, startTransfer, turns, waitFor,
} from './v2-receiver-orchestration-fixture'

afterEach(() => { vi.restoreAllMocks(); resetOrchestrationTestEnvironment() })

async function fixture() {
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const joined = new FakeJoinedShare(true)
  const controller = controllerFor(joined, receive)
  await startTransfer(controller, joined)
  const runtime = receive.startedAuthorities[0]?.runtime
  const run = joined.transferRuns[0]
  if (runtime === undefined || run === undefined) throw new Error('Receive did not start')
  const receiving = next(runtime.lifecycle, { kind: 'receiving', activeLeaseId: identityText(90) })
  return { controller, joined, runtime, run, receiving,
    task: () => composeTasks(controller.getSnapshot()).current }
}

describe('active receive lifecycle observation', () => {
  it('shows receiving before any bytes, then reconnecting and receiving again', async () => {
    const f = await fixture()
    expect(f.task()?.headline).toBe('Preparing download')
    f.runtime.lifecycleNotifications.publish(f.receiving)
    expect(f.controller.getSnapshot().progress.writtenBytes).toBe(0n)
    expect(f.task()?.headline).toBe('Downloading')
    expect(f.controller.getSnapshot().output.activeControls).toEqual(['pause', 'stop'])

    f.joined.connectionChanged({ kind: 'reconnecting', activity: { kind: 'connecting' } })
    expect(f.task()?.headline).toBe('Reconnecting to the sender')
    f.joined.connectionChanged({ kind: 'connected' })
    expect(f.task()?.headline).toBe('Downloading')
    await f.controller.dispose()
  })

  it('keeps writer ownership until the already-observed terminal state settles', async () => {
    const f = await fixture()
    f.runtime.lifecycleNotifications.publish(f.receiving)
    const finishing = next(f.receiving, { kind: 'finalizing-tree', activeLeaseId: identityText(90) })
    f.runtime.lifecycleNotifications.publish(finishing)
    expect(f.task()?.stage).toBe('finishing')
    const completed = stableLifecycle(finishing, 'download-started')
    f.runtime.lifecycleNotifications.publish(completed)
    await turns()
    expect(f.controller.getSnapshot().output.transferResultPresentation).toBeNull()
    expect(f.runtime.detachments).toEqual([])
    expect(f.run.signal?.aborted).toBe(false)

    f.run.resolve({ ...completed })
    await waitFor(() => f.runtime.detachments.length === 1)
    expect(f.controller.getSnapshot().output.transferResultPresentation).not.toBeNull()
    expect(f.controller.getSnapshot().output.activeControls).toEqual([])
    expect(f.controller.getSnapshot().error).toBeNull()
    expect(f.task()?.stage).toBe('handed-to-browser')
    await f.controller.dispose()
  })

  it('keeps Pausing and hides continuation until the observed pause cut settles', async () => {
    const f = await fixture()
    f.runtime.lifecycleNotifications.publish(f.receiving)
    const paused = stableLifecycle(f.receiving, 'resumable-receive')
    f.runtime.interrupt = (_control, transfer) => {
      f.runtime.lifecycleNotifications.publish(paused)
      f.run.resolve({ ...paused })
      transfer.abort(new DOMException('Paused by receiver', 'AbortError'))
    }
    f.controller.performLifecycleAction('pause')
    expect(f.task()?.headline).toBe('Pausing')
    expect(f.controller.getSnapshot().output.lifecyclePresentation?.actions).toEqual([])
    expect(f.runtime.detachments).toEqual([])
    await waitFor(() => f.controller.getSnapshot().output.transferResultPresentation !== null)
    expect(f.task()?.headline).toBe('Paused')
    expect(f.controller.getSnapshot().output.lifecyclePresentation?.actions.map(action => action.kind))
      .toContain('continue')

    f.controller.performLifecycleAction('continue')
    await waitFor(() => f.joined.transferRuns.length === 2)
    expect(f.task()?.headline).toBe('Downloading')
    expect(f.controller.getSnapshot().error).toBeNull()
    await f.controller.dispose()
  })

  it('rejects foreign, older and detached-attempt notifications, including after resume', async () => {
    const callbacks: ReceiveLifecycleListener[] = []
    const stops: Array<ReturnType<typeof vi.fn>> = []
    vi.spyOn(FakeBoundRuntime.prototype, 'subscribeLifecycle').mockImplementation(function (
      this: FakeBoundRuntime, listener,
    ) {
      callbacks.push(listener)
      const stop = vi.fn(this.lifecycleNotifications.subscribe(listener))
      stops.push(stop)
      return stop
    })
    const f = await fixture()
    f.runtime.lifecycleNotifications.publish(f.receiving)
    f.runtime.lifecycleNotifications.publish({ ...f.receiving, operationId: identityText(91), generation: 100n })
    f.runtime.lifecycleNotifications.publish({ ...f.receiving, receiveIntentDigest: identityText(92, 32), generation: 100n })
    f.runtime.lifecycleNotifications.publish(f.runtime.lifecycle)
    expect(f.controller.getSnapshot().output.lifecycle).toEqual(f.receiving)

    const paused = stableLifecycle(f.receiving, 'resumable-receive')
    f.run.resolve(paused)
    await waitFor(() => f.controller.getSnapshot().output.transferResultPresentation !== null)
    expect(stops[0]).toHaveBeenCalledOnce()
    f.controller.performLifecycleAction('continue')
    await waitFor(() => f.joined.transferRuns.length === 2)
    const current = f.controller.getSnapshot().output.lifecycle
    callbacks[0]!({ ...paused, generation: 100n })
    expect(f.controller.getSnapshot().output.lifecycle).toBe(current)

    await f.controller.dispose()
    expect(stops[1]).toHaveBeenCalledOnce()
    const disposed = f.controller.getSnapshot()
    callbacks[1]!({ ...paused, generation: 101n })
    expect(f.controller.getSnapshot()).toBe(disposed)
  })
})
