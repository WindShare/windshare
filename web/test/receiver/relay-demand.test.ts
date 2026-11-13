import { expect, it, vi } from 'vitest'
import { ReceiverRelaySet } from '../../src/receiver/relay-set'
import type { ProtocolGenerationCore, ReceiverSessionFactory } from '../../src/receiver/session-factory'

it('parks failed relay retries until demand and cancels a parked worker on shutdown', async () => {
  let wake: (() => void) | undefined
  const attachRelay = vi.fn(async () => { throw new Error('dormant sender fallback') })
  const initial = { relayBase: 'primary', relayLaneId: 1, relay: { close: async () => undefined }, session: {
    keys: { initialLaneEpoch: 1 }, isClosed: false, availableLaneIds: () => [1], retireLane: async () => undefined,
  } } as unknown as ProtocolGenerationCore
  const relays = new ReceiverRelaySet({ initial,
    factory: { relayBases: ['primary', 'dormant'], attachRelay } as unknown as ReceiverSessionFactory,
    admit: () => undefined, sleep: async () => undefined, now: () => 0, observe: () => undefined, failure: () => 'retry',
    waitForDemand: signal => new Promise<void>((resolve, reject) => {
      const abort = () => reject(signal.reason)
      signal.addEventListener('abort', abort, { once: true })
      wake = () => { signal.removeEventListener('abort', abort); resolve() }
    }),
  })
  relays.start()
  await vi.waitFor(() => expect(wake).toBeDefined())
  expect(attachRelay).toHaveBeenCalledTimes(1)
  const resume = wake!
  wake = undefined
  resume()
  await vi.waitFor(() => expect(wake).toBeDefined())
  expect(attachRelay).toHaveBeenCalledTimes(2)
  await relays.close()
  expect(attachRelay).toHaveBeenCalledTimes(2)
})
