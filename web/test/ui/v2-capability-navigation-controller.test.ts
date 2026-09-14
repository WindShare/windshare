import { afterEach, describe, expect, it, vi } from 'vitest'
import { encodeSuite02CapabilityKey } from '../../src/crypto/suite02-link'
import { V2ReceiverController, type V2ReceiverTraceEvent } from '../../src/ui/v2-controller'
import type { V2BrowserReceiverGateway } from '../../src/ui/v2-gateway'
import {
  FakeGateway, FakeJoinedShare, FakeReceiveComposition, WORKSPACE_ENVIRONMENT,
  deferred, resetOrchestrationTestEnvironment, startTransfer, turns, waitFor,
} from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

describe('receiver capability navigation', () => {
  it('opens a share from an already initialized portal', async () => {
    const fixture = await receiver()
    const { controller, joined, gateway, location } = fixture
    expect(controller.getSnapshot().phase).toBe('awaiting-key')
    controller.openLocation(location)
    await startTransfer(controller, joined)
    expect(gateway.joinCount).toBe(1)
    await controller.dispose()
  })

  it('preserves active and paused tasks when the same link or equivalent separate key is reopened', async () => {
    const { controller, joined, gateway, location, receive, decisions, key } = await receiver()
    controller.openLocation(location)
    await startTransfer(controller, joined)
    const originalIntent = controller.getSnapshot().output.receiveIntent
    controller.openLocation(location)
    await waitFor(() => decisions.includes('share-navigation-reused'))
    expect(gateway.joinCount).toBe(1)
    expect(controller.getSnapshot().output.receiveIntent).toBe(originalIntent)
    expect(joined.transferRuns[0]?.signal?.aborted).toBe(false)
    expect(receive.startedAuthorities[0]?.runtime?.detachments).toEqual([])

    controller.performLifecycleAction('pause')
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'resumable-receive')
    const paused = controller.getSnapshot().output.lifecycle
    decisions.length = 0
    controller.submitKey(key)
    await waitFor(() => decisions.includes('share-navigation-reused'))
    expect(controller.getSnapshot().output.lifecycle).toBe(paused)
    expect(controller.getSnapshot().output.receiveIntent).toBe(originalIntent)
    expect(gateway.joinCount).toBe(1)
    expect(joined.reconnectCount).toBe(0)
    joined.connectionChanged({ kind: 'reconnecting', activity: { kind: 'connecting' } })
    controller.openLocation(location)
    await waitFor(() => joined.reconnectCount === 1)
    expect(controller.getSnapshot().output.lifecycle).toBe(paused)
    expect(joined.transferRuns).toHaveLength(1)
    expect(receive.startedAuthorities[0]?.runtime?.detachments).toEqual([])
    await controller.dispose()
  })

  it.each(['ended', 'unavailable'] as const)('reauthenticates an %s session when its link is reopened', async kind => {
    const { controller, joined, gateway, location } = await receiver()
    controller.openLocation(location)
    await waitFor(() => controller.getSnapshot().phase === 'browsing')
    joined.connectionChanged(kind === 'ended'
      ? { kind, reason: 'share-stopped' } : { kind, reason: 'protocol-failed' })
    controller.openLocation(location)
    await waitFor(() => gateway.joinCount === 2 && controller.getSnapshot().phase === 'browsing')
    expect(joined.closeCount).toBe(1)
    await controller.dispose()
  })

  it('keeps active output ownership when another share link arrives', async () => {
    const { controller, joined, gateway, location, receive, decisions } = await receiver()
    controller.openLocation(location)
    await startTransfer(controller, joined)
    const intent = controller.getSnapshot().output.receiveIntent
    const other = await shareLocation(3)
    controller.openLocation(other.location)
    await waitFor(() => decisions.includes('share-navigation-blocked'))
    expect(controller.getSnapshot().error).toContain('keep it in Downloads')
    expect(controller.getSnapshot().output.receiveIntent).toBe(intent)
    expect(joined.transferRuns[0]?.signal?.aborted).toBe(false)
    expect(receive.startedAuthorities[0]?.runtime?.detachments).toEqual([])
    expect(gateway.joinCount).toBe(1)

    decisions.length = 0
    controller.openLocation(location)
    await waitFor(() => decisions.includes('share-navigation-reused'))
    expect(gateway.joinCount).toBe(1)
    await controller.dispose()
  })

  it('does not publish an old share after delayed cleanup overtakes a newer navigation', async () => {
    const first = new FakeJoinedShare(true)
    const second = new FakeJoinedShare(true)
    const third = new FakeJoinedShare(true)
    const gateway = new FakeGateway([first, second, third])
    const controller = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, {
      receive: new FakeReceiveComposition(WORKSPACE_ENVIRONMENT),
    })
    controller.initialize((await shareLocation(1)).location)
    await waitFor(() => controller.getSnapshot().phase === 'browsing')
    const cleanup = deferred<void>()
    const close = vi.spyOn(first, 'close').mockImplementation(() => cleanup.promise)
    const staleRoot = vi.spyOn(second, 'rootDirectory')
    controller.openLocation((await shareLocation(3)).location)
    await waitFor(() => close.mock.calls.length === 1)
    controller.openLocation((await shareLocation(5)).location)
    await waitFor(() => gateway.joinCount === 3 && controller.getSnapshot().phase === 'browsing')
    cleanup.resolve()
    await turns()
    expect(staleRoot).not.toHaveBeenCalled()
    expect(second.closeCount).toBe(1)
    await controller.dispose()
  })

  it('closes a candidate when disposal happens during selection construction', async () => {
    const { controller, location, joined, gateway } = await receiver()
    const snapshot = joined.selection.snapshot.bind(joined.selection)
    let disposed: Promise<void> | undefined
    vi.spyOn(joined.selection, 'snapshot').mockImplementationOnce(() => {
      disposed = controller.dispose()
      return snapshot()
    })
    controller.openLocation(location)
    await waitFor(() => joined.closeCount === 1)
    await disposed
    expect(gateway.joinCount).toBe(1)
    expect(controller.getSnapshot().share).toBeNull()
  })

  it('does not dial after disposal overtakes initial asynchronous cleanup', async () => {
    const { controller, location, gateway } = await receiver()
    controller.openLocation(location)
    await controller.dispose()
    controller.openLocation(location)
    controller.submitKey('after-dispose')
    expect(gateway.joinCount).toBe(0)
  })
})

async function shareLocation(seed: number) {
  const key = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(seed), new Uint8Array(16).fill(seed + 1))
  const pageUrl = `https://receiver.invalid/s/${key.shareId}`
  return { key: key.encoded, location: { pageUrl, capabilityInput: `${pageUrl}#${key.encoded}` } }
}

async function receiver() {
  const { location, key } = await shareLocation(1)
  const joined = new FakeJoinedShare(true)
  const gateway = new FakeGateway([joined, new FakeJoinedShare(true)])
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const decisions: string[] = []
  const controller = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, {
    receive,
    trace: { current: (event: V2ReceiverTraceEvent) => {
      if (event.name === 'receiver_experience' && event.transition === 'intent') decisions.push(event.action)
    } },
  })
  controller.initialize({ capabilityInput: null, pageUrl: location.pageUrl })
  return { controller, joined, gateway, receive, location, key, decisions }
}
