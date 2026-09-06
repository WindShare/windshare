import { describe, expect, it, vi } from 'vitest'
import {
  probeNativeObjectSupport,
  type NativeSupportWorker,
} from '../../src/output/origin-private/native-object/support'

class ProbeWorker implements NativeSupportWorker {
  static readonly instances: ProbeWorker[] = []
  static get latest(): ProbeWorker { return this.instances.at(-1)! }
  onmessage: NativeSupportWorker['onmessage'] = null
  onerror: NativeSupportWorker['onerror'] = null
  onmessageerror: NativeSupportWorker['onmessageerror'] = null
  terminate = vi.fn()
  constructor() { ProbeWorker.instances.push(this) }
}

describe('Dedicated Worker native capability probe', () => {
  it.each([true, false, 'true'] as const)('accepts only explicit worker support: %s', async reply => {
    const result = probeNativeObjectSupport({ Worker: ProbeWorker })
    ProbeWorker.latest.onmessage?.({ data: reply } as MessageEvent<unknown>)
    expect(await result).toBe(reply === true)
    expect(ProbeWorker.latest.terminate).toHaveBeenCalledOnce()
  })

  it('treats absent, blocked, and failed workers as unsupported', async () => {
    expect(await probeNativeObjectSupport({})).toBe(false)
    class BlockedWorker extends ProbeWorker {
      constructor() { super(); throw new Error('Worker blocked') }
    }
    expect(await probeNativeObjectSupport({ Worker: BlockedWorker })).toBe(false)
    const result = probeNativeObjectSupport({ Worker: ProbeWorker })
    ProbeWorker.latest.onerror?.({ message: 'Failed to load' } as ErrorEvent)
    expect(await result).toBe(false)
    expect(ProbeWorker.latest.terminate).toHaveBeenCalledOnce()
  })
})
