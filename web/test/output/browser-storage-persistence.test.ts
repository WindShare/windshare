import { describe, expect, it, vi } from 'vitest'
import { requestBrowserStoragePersistence } from '../../src/output/browser-storage/persistence'
import type { OutputTraceEvent } from '../../src/output/diagnostics/trace'

const FIRST_OPERATION = 'AQAAAAAAAAAAAAAAAAAAAA'
const SECOND_OPERATION = 'AgAAAAAAAAAAAAAAAAAAAA'

function observation() {
  const events: OutputTraceEvent<'storage_persistence'>[] = []
  const trace = { current: (event: OutputTraceEvent) => {
    if (event.eventName === 'storage_persistence') events.push(event)
  } }
  return { events, trace, transitions: () => events.map(event => event.payload.transition) }
}

function deferred() {
  let resolve!: (value: boolean) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<boolean>((accept, refuse) => { resolve = accept; reject = refuse })
  return { promise, resolve, reject }
}

describe('browser storage eviction protection', () => {
  it.each([true, false])('shares an unresolved request without giving tasks an awaitable dependency (grant %s)', async granted => {
    const pending = deferred()
    const storage = { persist: vi.fn(() => pending.promise) }
    const observed = observation()

    expect(requestBrowserStoragePersistence(storage, FIRST_OPERATION, observed.trace)).toBeUndefined()
    requestBrowserStoragePersistence(storage, SECOND_OPERATION, observed.trace)
    expect(storage.persist).toHaveBeenCalledOnce()
    expect(storage.persist.mock.contexts[0]).toBe(storage)
    expect(observed.transitions()).toEqual(['requested', 'already_pending'])
    expect(observed.events[1]?.payload).toMatchObject({
      operation_id: SECOND_OPERATION, request_operation_id: FIRST_OPERATION,
    })

    pending.resolve(granted)
    await pending.promise
    expect(observed.transitions()).toEqual(['requested', 'already_pending', granted ? 'granted' : 'not_granted'])
    // A later task must be able to use a permission changed in browser settings.
    requestBrowserStoragePersistence(storage, SECOND_OPERATION, observed.trace)
    await pending.promise
    expect(storage.persist).toHaveBeenCalledTimes(2)
  })

  it('does not share permission lifetime across different storage managers', () => {
    const first = { persist: vi.fn(() => new Promise<boolean>(() => {})) }
    const second = { persist: vi.fn(() => new Promise<boolean>(() => {})) }
    requestBrowserStoragePersistence(first, FIRST_OPERATION)
    requestBrowserStoragePersistence(second, SECOND_OPERATION)
    expect(first.persist).toHaveBeenCalledOnce()
    expect(second.persist).toHaveBeenCalledOnce()
  })

  it('observes rejection without an unhandled failure and permits another task to retry', async () => {
    const pending = deferred()
    const storage = { persist: vi.fn(() => pending.promise).mockResolvedValueOnce(true) }
    const observed = observation()
    requestBrowserStoragePersistence(storage, FIRST_OPERATION, observed.trace)
    await Promise.resolve()
    requestBrowserStoragePersistence(storage, SECOND_OPERATION, observed.trace)
    pending.reject(new DOMException('Storage is unavailable', 'SecurityError'))
    await pending.promise.catch(() => undefined)
    expect(observed.transitions()).toEqual(['requested', 'granted', 'requested', 'failed'])
    storage.persist.mockResolvedValueOnce(false)
    requestBrowserStoragePersistence(storage, SECOND_OPERATION, observed.trace)
    await Promise.resolve()
    expect(observed.transitions().at(-1)).toBe('not_granted')
  })

  it('treats an absent or synchronously failing permission API as optional', () => {
    const observed = observation()
    requestBrowserStoragePersistence({}, FIRST_OPERATION, observed.trace)
    const storage = { persist: vi.fn((): Promise<boolean> => { throw new Error('Storage unavailable') }) }
    expect(() => requestBrowserStoragePersistence(storage, FIRST_OPERATION, observed.trace)).not.toThrow()
    expect(() => requestBrowserStoragePersistence(storage, SECOND_OPERATION, observed.trace)).not.toThrow()
    expect(observed.transitions()).toEqual(['unavailable', 'requested', 'failed', 'requested', 'failed'])
  })

  it('keeps diagnostics from owning permission completion', async () => {
    const storage = { persist: vi.fn(async () => true) }
    const trace = { current: () => { throw new Error('Observer failed') } }
    expect(() => requestBrowserStoragePersistence(storage, FIRST_OPERATION, trace)).not.toThrow()
    await Promise.resolve()
    requestBrowserStoragePersistence(storage, SECOND_OPERATION, trace)
    await Promise.resolve()
    expect(storage.persist).toHaveBeenCalledTimes(2)
  })
})
