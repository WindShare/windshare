import { describe, expect, it } from 'vitest'
import { createBrowserTraceActivationStore } from '../../../src/diagnostics/browser-trace-activation'

describe('tab and share scoped diagnostic activation', () => {
  it('shares one deadline across reloads of the same share, independently of relays and fragments', () => {
    const storage = memoryStorage()
    const initial = createBrowserTraceActivationStore(() => storage, 'https://receiver.invalid/s/one?r=a#key')
    initial.writeExpiry(123)
    expect(createBrowserTraceActivationStore(() => storage, 'https://receiver.invalid/s/one?r=b').readExpiry()).toBe(123)
    expect(createBrowserTraceActivationStore(() => storage, 'https://receiver.invalid/s/two').readExpiry()).toBeUndefined()
    expect(createBrowserTraceActivationStore(memoryStorage, 'https://receiver.invalid/s/one').readExpiry()).toBeUndefined()
    initial.clear()
    expect(initial.readExpiry()).toBeUndefined()
  })
})

function memoryStorage() {
  const values = new Map<string, string>()
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value) },
    removeItem: (key: string) => { values.delete(key) },
  }
}
