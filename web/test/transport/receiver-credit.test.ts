import { describe, expect, it, vi } from 'vitest'

import { ReceiverCredit } from '../../src/transport/relay/receiver-credit'
import {
  encodeV2SessionCredit,
  V2_RELAY_SENDER_WINDOW_BYTES,
  V2_RELAY_SENDER_WINDOW_FRAMES,
} from '../../src/transport/relay/v2-protocol'

const relaySessionId = Uint8Array.of(1, 0, 0, 0, 0, 0, 0, 1)
const encodedFrameBytes = 21
const grant = (frames: number, bytes: number) => encodeV2SessionCredit({ relaySessionId, frames, bytes })

describe('receiver relay credit', () => {
  it('starts empty and waits for independent frame and wire-byte grants', async () => {
    const credit = new ReceiverCredit(relaySessionId)
    const lifetime = new AbortController()
    const ready = vi.fn()
    const pending = credit.waitForCapacity(encodedFrameBytes, lifetime.signal).then(ready)
    await Promise.resolve()
    expect(ready).not.toHaveBeenCalled()

    expect(credit.receive(grant(1, 0))).toBe(true)
    await Promise.resolve()
    expect(ready).not.toHaveBeenCalled()
    credit.receive(grant(0, encodedFrameBytes - 1))
    await Promise.resolve()
    expect(ready).not.toHaveBeenCalled()
    credit.receive(grant(0, 1))
    await pending
    credit.consume(encodedFrameBytes)
    expect(() => credit.consume(encodedFrameBytes)).toThrow(/available receiver credit/)
  })

  it('spends one frame and preserves surplus bytes for the next frame grant', async () => {
    const credit = new ReceiverCredit(relaySessionId)
    const signal = new AbortController().signal
    credit.receive(grant(1, 2 * encodedFrameBytes))
    await credit.waitForCapacity(encodedFrameBytes, signal)
    credit.consume(encodedFrameBytes)
    expect(() => credit.consume(encodedFrameBytes)).toThrow()
    const pending = credit.waitForCapacity(encodedFrameBytes, signal)
    credit.receive(grant(1, 0))
    await pending
    credit.consume(encodedFrameBytes)
  })

  it('cancels an empty-window wait and preserves subsequent grants', async () => {
    const credit = new ReceiverCredit(relaySessionId)
    const controller = new AbortController()
    const reason = new Error('caller canceled')
    const pending = credit.waitForCapacity(encodedFrameBytes, controller.signal)
    const rejected = expect(pending).rejects.toBe(reason)
    controller.abort(reason)
    await rejected
    credit.receive(grant(1, encodedFrameBytes))
    await credit.waitForCapacity(encodedFrameBytes, new AbortController().signal)
    credit.consume(encodedFrameBytes)
    await expect(credit.waitForCapacity(encodedFrameBytes, controller.signal)).rejects.toBe(reason)
  })

  it('rejects each aggregate dimension without partially applying the invalid grant', () => {
    const credit = new ReceiverCredit(relaySessionId)
    credit.receive(grant(V2_RELAY_SENDER_WINDOW_FRAMES, V2_RELAY_SENDER_WINDOW_BYTES))
    expect(() => credit.receive(grant(1, 0))).toThrow(/receiver window/)
    expect(() => credit.receive(grant(0, 1))).toThrow(/receiver window/)
    credit.consume(encodedFrameBytes)
    expect(credit.receive(grant(1, encodedFrameBytes))).toBe(true)
    expect(() => credit.receive(grant(1, 0))).toThrow(/receiver window/)
  })

  it('binds grants to a snapshot of the exact receiver session and ignores other frame kinds', () => {
    const identity = relaySessionId.slice()
    const credit = new ReceiverCredit(identity)
    identity[0] = 2
    expect(credit.receive(new Uint8Array(3))).toBe(false)
    expect(credit.receive(new TextEncoder().encode('WS2A'))).toBe(false)
    expect(() => credit.receive(encodeV2SessionCredit({ relaySessionId: identity, frames: 1, bytes: 0 })))
      .toThrow(/another receiver session/)
    expect(credit.receive(grant(1, encodedFrameBytes))).toBe(true)
  })

  it('rejects malformed wire credit before changing capacity', () => {
    const credit = new ReceiverCredit(relaySessionId)
    const malformed = grant(1, encodedFrameBytes)
    malformed[5] = 1
    expect(() => credit.receive(malformed)).toThrow(/WS2W/)
    expect(() => credit.consume(encodedFrameBytes)).toThrow()
    const zero = grant(1, 0)
    zero[19] = 0
    expect(() => credit.receive(zero)).toThrow(/session credit/)
  })
})
