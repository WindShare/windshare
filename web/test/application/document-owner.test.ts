import { describe, expect, it, vi } from 'vitest'
import { DocumentOwner, type DocumentRuntime } from '../../src/document/owner'

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function runtime(): DocumentRuntime {
  return { View: () => null, Downloads: () => null,
    surface: { subscribe: () => () => undefined, getSnapshot: () => true },
    openLocation: vi.fn(), ensureDownloads: vi.fn(async () => undefined), dispose: vi.fn(async () => undefined) }
}
const location = (capabilityInput: string | null) => ({ capabilityInput, pageUrl: 'https://receiver.test/' })

describe('document runtime ownership', () => {
  it('keeps the landing page unloaded and coalesces downloads with the latest incoming share', async () => {
    const module = deferred<() => DocumentRuntime>()
    const load = vi.fn(() => module.promise)
    const loaded = runtime()
    const owner = new DocumentOwner({ pageUrl: location(null).pageUrl, load, acceptDiagnostics: vi.fn(), record: vi.fn() })
    owner.openLocation(location(null))
    expect(load).not.toHaveBeenCalled()
    owner.setDownloadsOpen(true)
    owner.openLocation(location('first'))
    owner.openLocation(location('latest'))
    await Promise.resolve()
    expect(load).toHaveBeenCalledTimes(1)
    module.resolve(() => loaded)
    await vi.waitFor(() => expect(owner.getSnapshot().runtime).toBe(loaded))
    expect(loaded.openLocation).toHaveBeenCalledExactlyOnceWith(location('latest'))
    expect(loaded.ensureDownloads).toHaveBeenCalledTimes(1)
    expect(owner.getSnapshot().downloadsOpen).toBe(true)
    await owner.dispose()
    expect(loaded.dispose).toHaveBeenCalledTimes(1)
  })

  it('retries the captured capability in memory after module failure', async () => {
    const loaded = runtime()
    const load = vi.fn<() => Promise<() => DocumentRuntime>>()
      .mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce(() => loaded)
    const events: unknown[] = []
    const owner = new DocumentOwner({ pageUrl: location(null).pageUrl, load, acceptDiagnostics: vi.fn(), record: event => events.push(event) })
    owner.openLocation(location('erased-fragment'))
    await vi.waitFor(() => expect(owner.getSnapshot().failed).toBe(true))
    expect(owner.getSnapshot().view).toBe('opening')
    owner.retry()
    await vi.waitFor(() => expect(owner.getSnapshot().runtime).toBe(loaded))
    expect(loaded.openLocation).toHaveBeenCalledExactlyOnceWith(location('erased-fragment'))
    expect(events).toContainEqual(expect.objectContaining({ attempt: 2, intentId: 1, transition: 'ready' }))
    expect(JSON.stringify(events)).not.toContain('erased-fragment')
    await owner.dispose()
  })

  it('cannot recreate receiver ownership when a module resolves after document exit', async () => {
    const module = deferred<() => DocumentRuntime>()
    const create = vi.fn(() => runtime())
    const owner = new DocumentOwner({ pageUrl: location(null).pageUrl, load: () => module.promise, acceptDiagnostics: vi.fn(), record: vi.fn() })
    owner.openLocation(location('share'))
    await Promise.resolve()
    await owner.dispose()
    module.resolve(create)
    for (let turn = 0; turn < 8; turn++) await Promise.resolve()
    expect(create).not.toHaveBeenCalled()
    owner.retry()
    owner.setDownloadsOpen(true)
    owner.openLocation(location('another'))
    expect(create).not.toHaveBeenCalled()
  })
})
