import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  BrowserRelaySocketFactory,
  RELAY_INGRESS_CLOSE_CODE,
  RelaySocketConnectError,
  RelaySocketIngressError,
} from '../../src/transport/relay'
import { settle } from './helpers'

class BrowserWebSocketDouble extends EventTarget {
  readyState: number = WebSocket.CONNECTING
  binaryType: BinaryType = 'blob'
  bufferedAmount = 0
  readonly sent: Array<string | Uint8Array> = []
  readonly closes: Array<{ code?: number; reason?: string }> = []

  open(): void {
    this.readyState = WebSocket.OPEN
    this.dispatchEvent(new Event('open'))
  }

  failOpen(): void {
    this.dispatchEvent(new Event('error'))
  }

  closeImmediately = true

  message(data: unknown): void {
    this.dispatchEvent(new MessageEvent('message', { data }))
  }

  send(data: string | BufferSource | Blob): void {
    if (typeof data === 'string') {
      this.sent.push(data)
      return
    }
    if (data instanceof Blob) {
      throw new Error('test double does not accept Blob sends')
    }
    const view = ArrayBuffer.isView(data)
      ? new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
      : new Uint8Array(data)
    this.sent.push(view.slice())
  }

  close(code?: number, reason?: string): void {
    this.closes.push({ ...(code === undefined ? {} : { code }), ...(reason === undefined ? {} : { reason }) })
    if (this.readyState === WebSocket.CLOSED) {
      return
    }
    if (this.closeImmediately) {
      this.finishClose()
    }
  }

  finishClose(): void {
    if (this.readyState === WebSocket.CLOSED) {
      return
    }
    this.readyState = WebSocket.CLOSED
    this.dispatchEvent(new Event('close'))
  }
}

class RejectingBlob extends Blob {
  override arrayBuffer(): Promise<ArrayBuffer> {
    return Promise.reject(new Error('blob conversion failed'))
  }
}

class CountingBlob extends Blob {
  conversions = 0

  override arrayBuffer(): Promise<ArrayBuffer> {
    this.conversions += 1
    return super.arrayBuffer()
  }
}

afterEach(() => {
  vi.useRealTimers()
})

describe('browser WebSocket adapter', () => {
  it('redacts constructor diagnostics that can contain credential-bearing URLs', async () => {
    const factory = new BrowserRelaySocketFactory(() => {
      throw new Error('wss://user:secret@relay.example/?token=secret')
    })
    const failure = factory.connect('wss://relay.example')
    await expect(failure).rejects.toBeInstanceOf(RelaySocketConnectError)
    await expect(failure).rejects.not.toHaveProperty('cause')
    await expect(failure).rejects.not.toThrow(/secret/u)
  })

  it('physically closes a socket whose connection event fails', async () => {
    const browserSocket = new BrowserWebSocketDouble()
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.failOpen()
    await expect(connecting).rejects.toThrow(/closed during connection/u)
    expect(browserSocket.closes).toHaveLength(1)
    expect(browserSocket.readyState).toBe(WebSocket.CLOSED)
  })

  it('waits for open, snapshots binary sends, and preserves inbound order', async () => {
    const browserSocket = new BrowserWebSocketDouble()
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
    )
    const connecting = factory.connect('wss://relay.example/v1/ws/share')
    browserSocket.open()
    const socket = await connecting
    const outbound = Uint8Array.of(1, 2)
    await socket.sendBinary(outbound)
    outbound.fill(9)
    expect(browserSocket.sent).toEqual([Uint8Array.of(1, 2)])

    const reader = socket.messages.getReader()
    browserSocket.message('hello')
    browserSocket.message(Uint8Array.of(3, 4).buffer)
    expect(await reader.read()).toMatchObject({ value: { type: 'text', data: 'hello' } })
    expect(await reader.read()).toMatchObject({
      value: { type: 'binary', data: Uint8Array.of(3, 4) },
    })
    reader.releaseLock()
    await socket.close()
  })

  it('supports cancellation and recovery while bufferedAmount applies backpressure', async () => {
    vi.useFakeTimers()
    const browserSocket = new BrowserWebSocketDouble()
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
      { maxBufferedBytes: 2 },
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.open()
    const socket = await connecting
    browserSocket.bufferedAmount = 3
    const abort = new AbortController()
    const cancelled = socket.sendBinary(Uint8Array.of(1), abort.signal)
    abort.abort(new DOMException('cancelled', 'AbortError'))
    await expect(cancelled).rejects.toMatchObject({ name: 'AbortError' })

    const recovered = socket.sendBinary(Uint8Array.of(2))
    browserSocket.bufferedAmount = 0
    await vi.advanceTimersByTimeAsync(4)
    await recovered
    expect(browserSocket.sent).toEqual([Uint8Array.of(2)])
    await socket.close()
  })

  it('treats the configured high-water mark itself as backpressured', async () => {
    vi.useFakeTimers()
    const browserSocket = new BrowserWebSocketDouble()
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
      { maxBufferedBytes: 2 },
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.open()
    const socket = await connecting
    browserSocket.bufferedAmount = 2
    const send = socket.sendBinary(Uint8Array.of(1))
    await settle()
    expect(browserSocket.sent).toHaveLength(0)
    browserSocket.bufferedAmount = 1
    await vi.advanceTimersByTimeAsync(4)
    await send
    expect(browserSocket.sent).toEqual([Uint8Array.of(1)])
    await socket.close()
  })

  it('fails closed when the adapter ingress bound is exceeded', async () => {
    const browserSocket = new BrowserWebSocketDouble()
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
      { ingressMessages: 1 },
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.open()
    const socket = await connecting
    browserSocket.message('first')
    browserSocket.message('second')
    await settle()
    await expect(socket.messages.getReader().read()).rejects.toBeInstanceOf(
      RelaySocketIngressError,
    )
    expect(socket.state).toBe('closed')
    expect(browserSocket.closes.at(-1)?.code).toBe(RELAY_INGRESS_CLOSE_CODE)
  })

  it('lets failure cleanup callers await physical WebSocket settlement', async () => {
    const browserSocket = new BrowserWebSocketDouble()
    browserSocket.closeImmediately = false
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
      { ingressMessages: 1 },
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.open()
    const socket = await connecting
    browserSocket.message('first')
    browserSocket.message('second')
    await settle()
    const physicalClose = socket.close()
    let settled = false
    physicalClose.then(() => {
      settled = true
    }).catch(() => undefined)
    await settle()
    expect(settled).toBe(false)
    browserSocket.finishClose()
    await physicalClose
  })

  it('fails closed when Blob conversion rejects instead of poisoning the ingress chain', async () => {
    const browserSocket = new BrowserWebSocketDouble()
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.open()
    const socket = await connecting
    const reader = socket.messages.getReader()
    browserSocket.message(new RejectingBlob([Uint8Array.of(1)]))
    const queuedAfterFailure = new CountingBlob([Uint8Array.of(2)])
    browserSocket.message(queuedAfterFailure)
    await expect(reader.read()).rejects.toBeInstanceOf(RelaySocketIngressError)
    expect(socket.state).toBe('closed')
    expect(browserSocket.closes.at(-1)?.code).toBe(RELAY_INGRESS_CLOSE_CODE)
    expect(queuedAfterFailure.conversions).toBe(0)
  })

  it('rejects an oversized Blob before allocating its ArrayBuffer', async () => {
    const browserSocket = new BrowserWebSocketDouble()
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
      { maxBinaryMessageBytes: 4 },
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.open()
    const socket = await connecting
    const reader = socket.messages.getReader()
    const blob = new CountingBlob([Uint8Array.of(1, 2, 3, 4, 5)])
    browserSocket.message(blob)
    await expect(reader.read()).rejects.toBeInstanceOf(RelaySocketIngressError)
    expect(blob.conversions).toBe(0)
  })

  it('makes concurrent close callers wait for the same physical settlement', async () => {
    const browserSocket = new BrowserWebSocketDouble()
    browserSocket.closeImmediately = false
    const factory = new BrowserRelaySocketFactory(
      () => browserSocket as unknown as WebSocket,
    )
    const connecting = factory.connect('wss://relay.example')
    browserSocket.open()
    const socket = await connecting
    const first = socket.close()
    const second = socket.close()
    let secondSettled = false
    second.then(() => {
      secondSettled = true
    }).catch(() => undefined)
    await settle()
    expect(secondSettled).toBe(false)
    browserSocket.finishClose()
    await Promise.all([first, second])
  })
})
