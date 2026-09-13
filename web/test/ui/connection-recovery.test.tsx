import { afterEach, expect, it, vi } from 'vitest'
import { renderToString } from 'react-dom/server'
import { V2ReceiverApp } from '../../src/ui/V2ReceiverApp'
import { composeTasks } from '../../src/ui/experience/task-composition'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type { V2BrowserReceiverGateway, V2JoinedBrowserShare } from '../../src/ui/v2-gateway'
import { runInitialJoin, type InitialJoinOptions } from '../../src/receiver/initial-join'
import { experienceController, experienceSnapshot } from './receiver-experience-fixture'
import { FakeReceiveComposition, FakeJoinedShare, WORKSPACE_ENVIRONMENT, controllerFor,
  startTransfer, next, identityText, waitFor, turns, resetOrchestrationTestEnvironment } from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

it('extends initial waiting through the existing gateway call and releases it when cancelled', async () => {
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const joined = new FakeJoinedShare(true)
  let available = false
  let now = 0
  const gateway = { join: vi.fn(async (_input: string, _page: string, signal: AbortSignal, recovery: InitialJoinOptions) =>
    runInitialJoin({ ...recovery, signal, windowMilliseconds: 100,
      clock: { now: () => now, sleep: async delay => { now += delay } },
      connect: async () => {
        if (!available) throw new Error('Sender is offline')
        return joined as unknown as V2JoinedBrowserShare
      }, close: value => value.close(),
    })) }
  const controller = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, { receive })
  controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
  await waitFor(() => controller.getSnapshot().connection.kind === 'idle' &&
    'join' in controller.getSnapshot().connection && controller.getSnapshot().status.includes('temporarily unreachable'))
  expect(gateway.join).toHaveBeenCalledTimes(1)
  available = true
  controller.continueJoinWaiting()
  await waitFor(() => controller.getSnapshot().phase === 'browsing')
  expect(gateway.join).toHaveBeenCalledTimes(1)
  expect(controller.getSnapshot().connection).toEqual({ kind: 'connected' })
  await controller.dispose()

  available = false
  const cancelled = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, { receive })
  cancelled.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
  await waitFor(() => cancelled.getSnapshot().status.includes('temporarily unreachable'))
  const attempts = gateway.join.mock.calls.length
  cancelled.cancelJoin()
  await turns()
  expect(cancelled.getSnapshot().phase).toBe('awaiting-key')
  expect(gateway.join.mock.calls.at(-1)![2].aborted).toBe(true)
  cancelled.continueJoinWaiting()
  await turns()
  expect(gateway.join).toHaveBeenCalledTimes(attempts)
  await cancelled.dispose()
})

it('reconnects the joined share without replacing an active download, selection, or output authority', async () => {
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const joined = new FakeJoinedShare(true, [], 'tree')
  const controller = controllerFor(joined, receive)
  await startTransfer(controller, joined)
  joined.connectionChanged({ kind: 'reconnecting', phase: 'waiting' })
  const before = controller.getSnapshot()
  const selection = joined.selection
  controller.requestReconnect()
  controller.requestReconnect()
  await turns()
  expect(joined.reconnectCount).toBe(2)
  expect(joined.closeCount).toBe(0)
  expect(joined.transferRuns).toHaveLength(1)
  expect(joined.transferRuns[0]!.signal?.aborted).toBe(false)
  expect(joined.selection).toBe(selection)
  const after = controller.getSnapshot()
  expect(after.output).toBe(before.output)
  expect(after.progress).toBe(before.progress)
  expect(after.activeReceiveOperationId).toBe(before.activeReceiveOperationId)
  joined.connectionChanged({ kind: 'connected' })
  expect(controller.getSnapshot().activeReceiveOperationId).toBe(before.activeReceiveOperationId)
  await controller.dispose()
})

it('keeps a paused task paused when the connection returns or the user requests a reconnect', async () => {
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const joined = new FakeJoinedShare(true, [], 'tree')
  const controller = controllerFor(joined, receive)
  await startTransfer(controller, joined)
  const runtime = receive.startedAuthorities[0]!.runtime!
  const paused = next(runtime.lifecycle, { kind: 'resumable-receive', payloadKind: 'opfs-zip',
    objectId: identityText(92), checkpointGeneration: 2n, occupiedBytes: 4096n,
    completedFileCount: 1n, completedBytes: 1024n, discoveryComplete: false })
  joined.transferRuns[0]!.resolve(paused)
  await waitFor(() => controller.canRetainCurrentOperation)
  joined.connectionChanged({ kind: 'reconnecting', phase: 'waiting' })
  controller.requestReconnect()
  joined.connectionChanged({ kind: 'connected' })
  await turns()
  const snapshot = controller.getSnapshot()
  expect(snapshot.output.lifecycle).toBe(paused)
  expect(joined.transferRuns).toHaveLength(1)
  expect(composeTasks(snapshot).current?.stage).toBe('paused')
  await controller.dispose()
})

it('offers bounded initial waiting choices and same-page recovery controls', () => {
  const waiting = experienceSnapshot({ phase: 'joining', connection: { kind: 'idle', join: 'waiting-for-choice' },
    status: 'The sender is temporarily unreachable. The link may still be valid.' })
  const html = renderToString(<V2ReceiverApp controller={experienceController(waiting)} />)
  expect(html).toContain('Retry now')
  expect(html).toContain('Continue waiting')
  expect(html).toContain('Cancel')
  expect(html).toContain('The link may still be valid')
  const reconnecting = renderToString(<V2ReceiverApp controller={experienceController(experienceSnapshot({
    phase: 'browsing', connection: { kind: 'reconnecting', phase: 'waiting' },
  }))} />)
  expect(reconnecting).toContain('Reconnect now')
  expect(reconnecting).toContain('preserve your download progress')
  expect(reconnecting).not.toContain('reopen the original link')
})
