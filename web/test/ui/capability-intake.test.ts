import { describe, expect, it, vi } from 'vitest'
import { BrowserCapabilityIntake } from '../../src/ui/capability/intake'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import { V2BrowserReceiverGateway } from '../../src/ui/v2-gateway'
import { INERT_TEST_RECEIVE_COMPOSITION } from './v2-receive-fixture'

describe('capability presentation intake', () => {
  it('activates direct and pasted links before the first connection milestone', async () => {
    const start = vi.fn()
    const intake = new BrowserCapabilityIntake(start)
    const observed: number[] = []
    const controller = new V2ReceiverController(new V2BrowserReceiverGateway(), {
      receive: INERT_TEST_RECEIVE_COMPOSITION,
      capabilityIntake: intake,
      trace: { current: event => {
        if (event.name === 'join_transition' && event.transition === 'started') observed.push(start.mock.calls.length)
      } },
    })
    controller.initialize({ pageUrl: 'https://receiver.invalid/', capabilityInput: null, diagnosticsRequested: true })
    expect(start).toHaveBeenCalledTimes(1)
    controller.submitKey('https://receiver.invalid/share?trace=1#invalid')
    await vi.waitFor(() => expect(observed).toEqual([2]))
    await controller.dispose()
  })

  it('leaves ordinary links, bare keys and malformed input to capability validation', () => {
    const start = vi.fn()
    const intake = new BrowserCapabilityIntake(start)
    for (const input of ['key', 'not a URL', 'https://receiver.invalid/share#trace=1', 'https://receiver.invalid/share?trace=0#key']) {
      intake.accept({ capabilityInput: input, pageUrl: 'https://receiver.invalid/' })
    }
    expect(start).not.toHaveBeenCalled()
  })

  it('does not let unavailable presentation consumers prevent joining', async () => {
    const controller = new V2ReceiverController(new V2BrowserReceiverGateway(), {
      receive: INERT_TEST_RECEIVE_COMPOSITION,
      capabilityIntake: { accept() { throw new Error('presentation unavailable') } },
    })
    controller.initialize({ pageUrl: 'https://receiver.invalid/', capabilityInput: null })
    controller.submitKey('invalid-key')
    await vi.waitFor(() => expect(controller.getSnapshot().phase).toBe('failed'))
    await controller.dispose()
  })
})
