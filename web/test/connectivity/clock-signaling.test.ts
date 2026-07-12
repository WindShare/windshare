import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  BrowserConnectivityClock,
  RelaySignalingRoute,
  type ConnectivitySignal,
} from '../../src/connectivity'

afterEach(() => {
  vi.useRealTimers()
})

describe('connectivity clock', () => {
  it('resolves only at the injected deadline and removes its timer', async () => {
    vi.useFakeTimers()
    const clock = new BrowserConnectivityClock()
    let settled = false
    const sleeping = clock.sleep(10_000).then(() => {
      settled = true
    })

    await vi.advanceTimersByTimeAsync(9_999)
    expect(settled).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    await sleeping
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rejects immediately or while waiting with the caller abort reason', async () => {
    vi.useFakeTimers()
    const clock = new BrowserConnectivityClock()
    const before = new AbortController()
    before.abort(new DOMException('before', 'AbortError'))
    await expect(clock.sleep(1, before.signal)).rejects.toMatchObject({ name: 'AbortError' })

    const during = new AbortController()
    const sleeping = clock.sleep(10_000, during.signal)
    during.abort(new DOMException('during', 'AbortError'))
    await expect(sleeping).rejects.toMatchObject({ name: 'AbortError' })
    expect(vi.getTimerCount()).toBe(0)
  })
})

describe('relay signaling projection', () => {
  it('snapshots outbound payloads and exposes the session-scoped inbound stream', async () => {
    const channel = new FakeRelayChannel()
    const route = new RelaySignalingRoute(channel)
    const payload = { candidate: 'one', nested: { value: 1 } }
    await route.send({ kind: 'candidate', payload })
    payload.nested.value = 9
    expect(channel.sent).toEqual([
      { kind: 'candidate', payload: { candidate: 'one', nested: { value: 1 } } },
    ])

    const reading = route.messages.getReader().read()
    channel.push({ kind: 'answer', payload: { type: 'answer', sdp: 'v=0' } })
    await expect(reading).resolves.toMatchObject({
      done: false,
      value: { kind: 'answer', payload: { type: 'answer', sdp: 'v=0' } },
    })
    await channel.close()
  })

  it('rejects an empty outbound kind before touching the relay', async () => {
    const channel = new FakeRelayChannel()
    const route = new RelaySignalingRoute(channel)
    await expect(route.send({ kind: '', payload: {} })).rejects.toBeInstanceOf(TypeError)
    expect(channel.sent).toHaveLength(0)
    await channel.close()
  })
})

class FakeRelayChannel {
  readonly frames: ReadableStream<Uint8Array>
  readonly signalMessages: ReadableStream<ConnectivitySignal>
  readonly sent: ConnectivitySignal[] = []
  state: 'open' | 'closed' = 'open'
  #frameController!: ReadableStreamDefaultController<Uint8Array>
  #signalController!: ReadableStreamDefaultController<ConnectivitySignal>

  constructor() {
    this.frames = new ReadableStream({
      start: (controller) => {
        this.#frameController = controller
      },
    })
    this.signalMessages = new ReadableStream({
      start: (controller) => {
        this.#signalController = controller
      },
    })
  }

  send(): Promise<void> {
    return Promise.resolve()
  }

  sendTerminal(): Promise<void> {
    return this.close()
  }

  sendSignal(kind: string, payload: unknown, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    this.sent.push(structuredClone({ kind, payload }))
    return Promise.resolve()
  }

  push(signal: ConnectivitySignal): void {
    this.#signalController.enqueue(structuredClone(signal))
  }

  close(): Promise<void> {
    if (this.state === 'open') {
      this.state = 'closed'
      this.#frameController.close()
      this.#signalController.close()
    }
    return Promise.resolve()
  }
}
