import type { Suite02CapabilityKey } from '../../src/crypto/suite02-link'
import type { V2WebSocketPort } from '../../src/transport/relay/v2-receiver'
import { decodeV2OpaqueRoute, encodeV2DescriptorDelivery, encodeV2SessionCredit } from '../../src/transport/relay/v2-protocol'

export const relaySessionId = Uint8Array.of(1, 0, 0, 0, 0, 0, 0, 1)
export const capability: Suite02CapabilityKey = Object.freeze({
  suite: 2,
  readSecret: new Uint8Array(16).fill(1),
  pkHash: new Uint8Array(16).fill(2),
  shareIdRaw: new Uint8Array(12).fill(3),
  shareId: 'share',
})

export class FakeSocket implements V2WebSocketPort {
  binaryType: BinaryType = 'arraybuffer'
  readyState = 1
  bufferedAmount = 0
  closeCode: number | undefined
  readonly sent: Uint8Array<ArrayBuffer>[] = []
  sendFailure: Error | undefined
  initialCredit: { frames: number; bytes: number } | undefined
  readonly #listeners = new Map<string, Set<(event: unknown) => void>>()
  readonly #descriptor: boolean

  constructor(descriptor = true) {
    this.#descriptor = descriptor
  }

  send(data: ArrayBufferView<ArrayBuffer>): void {
    if (this.sendFailure !== undefined) throw this.sendFailure
    const encoded = new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
    this.sent.push(encoded.slice())
    if (String.fromCharCode(...encoded.subarray(0, 4)) !== 'WS2J' || !this.#descriptor) return
    queueMicrotask(() => {
      this.message(encodeV2DescriptorDelivery({ relaySessionId, object: Uint8Array.of(1) }))
      if (this.initialCredit !== undefined) this.grant(this.initialCredit.frames, this.initialCredit.bytes)
    })
  }

  close(code?: number): void {
    if (code !== undefined && code !== 1000 && (code < 3000 || code > 4999)) {
      throw new DOMException('Invalid browser close code', 'InvalidAccessError')
    }
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

  grant(frames: number, bytes: number): void {
    this.message(encodeV2SessionCredit({ relaySessionId, frames, bytes }))
  }

  fail(): void {
    this.#emit('error', {})
  }

  get opaqueFrames(): Uint8Array[] {
    return this.sent
      .filter(encoded => String.fromCharCode(...encoded.subarray(0, 4)) === 'WS2O')
      .map(encoded => decodeV2OpaqueRoute(encoded).ciphertext)
  }

  #emit(type: string, event: unknown): void {
    // DOM dispatch does not invoke listeners added while the event is executing.
    for (const listener of [...this.#listeners.get(type) ?? []]) listener(event)
  }
}
