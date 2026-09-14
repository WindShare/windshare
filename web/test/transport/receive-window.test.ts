import { describe, expect, it, vi } from 'vitest'
import { RelayReceiveWindow } from '../../src/transport/relay/receive-window'
import {
  decodeReceiveCredit, encodeReceiveCredit, RELAY_OPAQUE_ROUTE_HEADER_BYTES,
  RELAY_RECEIVE_WINDOW_BYTES, RELAY_RECEIVE_WINDOW_FRAMES, RELAY_RECEIVE_CREDIT_BATCH_FRAMES,
} from '../../src/transport/relay/receive-credit-codec'

const relaySessionId = Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8)

describe('receiver-owned relay delivery credit', () => {
  it('matches the Go wire contract and rejects malformed grants', () => {
    const credit = { relaySessionId, frames: RELAY_RECEIVE_WINDOW_FRAMES, bytes: RELAY_RECEIVE_WINDOW_BYTES }
    const encoded = encodeReceiveCredit(credit)
    expect(Buffer.from(encoded).toString('hex')).toBe('575332420200000001020304050607080000010001000000')
    expect(decodeReceiveCredit(encoded)).toEqual(credit)
    for (const invalid of [
      { ...credit, relaySessionId: new Uint8Array(8) },
      { ...credit, relaySessionId: new Uint8Array(9) },
      { ...credit, frames: -1 }, { ...credit, frames: 0.5 },
      { ...credit, frames: RELAY_RECEIVE_WINDOW_FRAMES + 1 },
      { ...credit, bytes: RELAY_RECEIVE_WINDOW_BYTES + 1 },
      { ...credit, frames: 0, bytes: 0 },
    ]) expect(() => encodeReceiveCredit(invalid)).toThrow()
    for (const offset of [0, 4, 5, 6, 7, 16, 20]) {
      const invalid = encoded.slice()
      invalid[offset] = 255
      expect(() => decodeReceiveCredit(invalid)).toThrow()
    }
    expect(() => decodeReceiveCredit(encoded.subarray(1))).toThrow()
  })

  it('stops a cooperative producer during a stalled consumer and resumes in bounded batches', async () => {
    const frame = new Uint8Array(65_536)
    const wireBytes = frame.byteLength + RELAY_OPAQUE_ROUTE_HEADER_BYTES
    const total = RELAY_RECEIVE_WINDOW_FRAMES * 3
    let availableFrames = 0
    let availableBytes = 0
    let sent = 0
    const grants: number[] = []
    const queue = new RelayReceiveWindow(relaySessionId, encoded => {
      const grant = decodeReceiveCredit(encoded)
      availableFrames += grant.frames
      availableBytes += grant.bytes
      grants.push(grant.frames)
    }, () => undefined)
    const produce = () => {
      while (sent < total && availableFrames > 0 && availableBytes >= wireBytes) {
        availableFrames -= 1
        availableBytes -= wireBytes
        sent += 1
        queue.push(frame)
      }
    }
    queue.start()
    produce()
    expect(sent).toBe(Math.floor(RELAY_RECEIVE_WINDOW_BYTES / wireBytes))
    await Promise.resolve()
    expect(grants).toEqual([RELAY_RECEIVE_WINDOW_FRAMES])
    const reader = queue.stream.getReader()
    for (let index = 0; index < total; index += 1) {
      expect((await reader.read()).value).toBe(frame)
      produce()
    }
    expect(sent).toBe(total)
    expect(grants.slice(1).every(count => count === RELAY_RECEIVE_CREDIT_BATCH_FRAMES)).toBe(true)
    queue.close()
    expect((await reader.read()).done).toBe(true)
  })

  it('keeps short replies live without waiting for a credit flush', async () => {
    const grants = vi.fn()
    const queue = new RelayReceiveWindow(relaySessionId, grants, () => undefined)
    queue.start()
    const reader = queue.stream.getReader()
    for (let index = 0; index < RELAY_RECEIVE_CREDIT_BATCH_FRAMES - 1; index += 1) {
      const waiting = reader.read()
      queue.push(Uint8Array.of(index))
      expect((await waiting).value).toEqual(Uint8Array.of(index))
    }
    expect(grants).toHaveBeenCalledTimes(1)
    queue.close()
    expect((await reader.read()).done).toBe(true)
  })

  it('rejects delivery without credit and stops returning capacity after retirement', async () => {
    const grants = vi.fn()
    const cancel = vi.fn()
    const queue = new RelayReceiveWindow(relaySessionId, grants, cancel)
    expect(() => queue.push(Uint8Array.of(1))).toThrow(/receive credit/)
    queue.start()
    for (let index = 0; index < RELAY_RECEIVE_WINDOW_FRAMES; index += 1) queue.push(Uint8Array.of(index))
    expect(() => queue.push(Uint8Array.of(1))).toThrow(/receive credit/)
    queue.close()
    const reader = queue.stream.getReader()
    for (let index = 0; index < RELAY_RECEIVE_WINDOW_FRAMES; index += 1) await reader.read()
    expect(grants).toHaveBeenCalledTimes(1)
    expect((await reader.read()).done).toBe(true)

    const cancelled = new RelayReceiveWindow(relaySessionId, grants, cancel)
    cancelled.start()
    cancelled.push(Uint8Array.of(1))
    await cancelled.stream.cancel()
    expect(cancel).toHaveBeenCalledOnce()
    const failed = new RelayReceiveWindow(relaySessionId, grants, () => undefined)
    failed.start()
    failed.fail(new Error('disconnected'))
    await expect(failed.stream.getReader().read()).rejects.toThrow('disconnected')
  })
})
