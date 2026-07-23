import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Suite02CapabilityKey } from '../../src/crypto/suite02-link'
import {
  dialV2RelayReceiver,
  V2_RELAY_RECEIVE_QUEUE_FRAMES,
  type V2WebSocketPort,
} from '../../src/transport/relay/v2-receiver'
import {
  encodeV2DescriptorDelivery,
  encodeV2OpaqueRoute,
  encodeV2SessionRetired,
} from '../../src/transport/relay/v2-protocol'

const relaySessionId = Uint8Array.of(1, 0, 0, 0, 0, 0, 0, 1)
const capability: Suite02CapabilityKey = Object.freeze({
  suite: 2,
  readSecret: new Uint8Array(16).fill(1),
  pkHash: new Uint8Array(16).fill(2),
  shareIdRaw: new Uint8Array(12).fill(3),
  shareId: 'share',
})

class FakeSocket implements V2WebSocketPort {
  binaryType: BinaryType = 'arraybuffer'
  readyState = 1
  bufferedAmount = 0
  closeCode: number | undefined
  readonly #listeners = new Map<string, Set<(event: unknown) => void>>()
  readonly #descriptor: boolean

  constructor(descriptor = true) {
    this.#descriptor = descriptor
  }

  send(): void {
    if (!this.#descriptor) return
    queueMicrotask(() => this.message(encodeV2DescriptorDelivery({
      relaySessionId,
      object: Uint8Array.of(1),
    })))
  }

  close(code?: number): void {
    this.closeCode = code
    this.readyState = 3
    this.#emit('close', {})
  }

  addEventListener<K extends keyof WebSocketEventMap>(
    type: K,
    listener: (event: WebSocketEventMap[K]) => void,
  ): void {
    const listeners = this.#listeners.get(type) ?? new Set()
    listeners.add(listener as (event: unknown) => void)
    this.#listeners.set(type, listeners)
  }

  removeEventListener<K extends keyof WebSocketEventMap>(
    type: K,
    listener: (event: WebSocketEventMap[K]) => void,
  ): void {
    this.#listeners.get(type)?.delete(listener as (event: unknown) => void)
  }

  message(bytes: Uint8Array): void {
    const data = bytes.slice().buffer
    this.#emit('message', { data })
  }

  #emit(type: string, event: unknown): void {
    for (const listener of this.#listeners.get(type) ?? []) listener(event)
  }
}

afterEach(() => vi.useRealTimers())

describe('v2 relay receiver ingress', () => {
  it('fails the lane instead of buffering beyond its ciphertext frame budget', async () => {
    const socket = new FakeSocket()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const opaque = encodeV2OpaqueRoute({
      relaySessionId,
      ciphertext: new Uint8Array(65_536).fill(7),
    })

    for (let index = 0; index < V2_RELAY_RECEIVE_QUEUE_FRAMES; index += 1) {
      socket.message(opaque)
    }
    expect(socket.closeCode).toBeUndefined()
    socket.message(opaque)
    expect(socket.closeCode).toBe(1002)

    const reader = connection.channel.frames.getReader()
    await expect(reader.read()).rejects.toThrow(/receive queue/)
  })

  it('bounds a relay that opens but withholds descriptor delivery', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket(false)
    const pending = dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const rejected = expect(pending).rejects.toMatchObject({ name: 'TimeoutError' })
    for (let turn = 0; turn < 8; turn += 1) await Promise.resolve()
    await vi.advanceTimersByTimeAsync(30_000)

    await rejected
  })

  it('retires only the exact receiver channel and treats stale retirement as a no-op', async () => {
    const socket = new FakeSocket()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const reader = connection.channel.frames.getReader()

    socket.message(encodeV2SessionRetired({
      relaySessionId: Uint8Array.of(2, 0, 0, 0, 0, 0, 0, 1),
    }))
    expect(connection.channel.state).toBe('open')
    expect(socket.closeCode).toBeUndefined()

    socket.message(encodeV2SessionRetired({ relaySessionId }))
    expect(connection.channel.state).toBe('closed')
    expect(socket.closeCode).toBe(1000)
    await expect(reader.read()).resolves.toEqual({ done: true, value: undefined })

    socket.message(encodeV2SessionRetired({ relaySessionId }))
    expect(socket.closeCode).toBe(1000)
  })

  it('fails the receiver link on a malformed retirement control', async () => {
    const socket = new FakeSocket()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const malformed = encodeV2SessionRetired({ relaySessionId })
    malformed[5] = 1

    socket.message(malformed)
    expect(socket.closeCode).toBe(1002)
    await expect(connection.channel.frames.getReader().read()).rejects.toThrow(/WS2F/)
  })
})
