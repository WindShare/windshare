import { describe, expect, it, vi } from 'vitest'
import { admittedRelay, RelayPolicyDeniedError, type RelayAdmission } from '../../src/receiver/relay-admission'
import type { RelayReceiverConnection } from '../../src/transport/relay/receiver'
import { relayEndpointForShare } from '../../src/transport/relay/endpoint'

async function connection(): Promise<RelayReceiverConnection> {
  const unavailable = new AbortController()
  const close = vi.fn(async () => { unavailable.abort() })
  return {
    endpoint: await relayEndpointForShare('https://relay.example/v2/ws', new Uint8Array(12).fill(1)),
    relaySessionId: new Uint8Array(8).fill(1), descriptorObject: new Uint8Array(),
    channel: { state: 'open', unavailable: unavailable.signal, frames: new ReadableStream(),
      send: async () => undefined, sendTerminal: async () => undefined, close },
    close,
  }
}
function policy(beforeDial?: Promise<void>) {
  let denied = false
  const listeners = new Set<() => void>()
  const admission: RelayAdmission = {
    watch: (base, deny) => {
      const enforce = () => {
        if (denied && base === 'official') deny(new RelayPolicyDeniedError())
      }
      listeners.add(enforce)
      enforce()
      return { beforeDial, close: () => { listeners.delete(enforce) } }
    },
  }
  return { admission, listeners, block: () => { denied = true; [...listeners].forEach(listener => listener()) } }
}
describe('relay policy lifetime', () => {
  it('closes only official relay connections and prevents their next dial', async () => {
    const scope = policy(), official = await connection(), independent = await connection()
    await admittedRelay(scope.admission, 'official', new AbortController().signal, async () => official)
    await admittedRelay(scope.admission, 'self-hosted', new AbortController().signal, async () => independent)
    scope.block()
    expect(official.close).toHaveBeenCalledTimes(1)
    expect(independent.close).not.toHaveBeenCalled()
    const dial = vi.fn(async () => official)
    await expect(admittedRelay(scope.admission, 'official', new AbortController().signal, dial)).rejects.toBeInstanceOf(RelayPolicyDeniedError)
    expect(dial).not.toHaveBeenCalled()
    await independent.close()
    expect(scope.listeners.size).toBe(0)
  })
  it('retires a late dial result when policy changes in flight', async () => {
    const scope = policy(), relay = await connection()
    let resolve!: (value: RelayReceiverConnection) => void
    const pending = new Promise<RelayReceiverConnection>(done => { resolve = done })
    const dial = vi.fn(() => pending)
    const task = admittedRelay(scope.admission, 'official', new AbortController().signal, dial)
    expect(dial).toHaveBeenCalledTimes(1)
    scope.block()
    resolve(relay)
    await expect(task).rejects.toBeInstanceOf(RelayPolicyDeniedError)
    expect(relay.close).toHaveBeenCalledTimes(1)
    expect(scope.listeners.size).toBe(0)
  })
  it('cancels local cache preparation without dialing or retaining a policy listener', async () => {
    const scope = policy(new Promise<void>(() => undefined)), cancel = new AbortController()
    const dial = vi.fn(connection)
    const task = admittedRelay(scope.admission, 'official', cancel.signal, dial)
    cancel.abort(new Error('stopped'))
    await expect(task).rejects.toThrow('stopped')
    expect(dial).not.toHaveBeenCalled()
    expect(scope.listeners.size).toBe(0)
  })
})
