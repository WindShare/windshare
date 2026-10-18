import { describe, expect, it } from 'vitest'
import { decodeDeliveryConsumed, encodeDeliveryConsumed } from '../../../src/transport/relay/delivery-consumed-codec'
import type { ReceiveReceipt } from '../../../src/transport/relay/receive-ledger'
import { appendEnvelope, MAX_UINT64 } from '../../../src/transport/relay/receive-ledger/counts'
import { createLedger, MAX_ENVELOPE_BYTES, sessionId, ZERO } from './fixtures'

describe('connection receive ledger', () => {
  it('shares both capacity axes and refunds only when feedback is handed to the writer', () => {
    const ledger = createLedger({ capacity: { frames: 2n, encodedBytes: 50n } })
    const first = ledger.open(sessionId(1))
    const second = ledger.open(sessionId(2))
    const a = ledger.receive(first, 21)
    const b = ledger.receive(second, 29)
    const full = ledger.usage()
    expect(full).toEqual({ held: { frames: 2n, encodedBytes: 50n }, outstanding: full.held, pendingConsumed: ZERO, retainedSessions: 2 })
    expect(ledger.takeConsumed()).toBeUndefined()
    expect(() => ledger.receive(second, 21)).toThrow('capacity')
    expect(ledger.usage()).toEqual(full)

    ledger.consume(a)
    expect(ledger.usage().held).toEqual({ frames: 1n, encodedBytes: 29n })
    expect(() => ledger.receive(second, 21)).toThrow('capacity')
    const feedback = ledger.takeConsumed()!
    expect(decodeDeliveryConsumed(encodeDeliveryConsumed(feedback))).toEqual({
      relaySessionId: sessionId(1), totalFrames: 1n, totalEncodedBytes: 21n,
    })
    expect(() => ledger.receive(first, 22)).toThrow('capacity')
    ledger.receive(first, 21)
    ledger.consume(b)
    expect(ledger.takeConsumed()).toEqual({ relaySessionId: sessionId(2), totalFrames: 1n, totalEncodedBytes: 29n })
    expect(ledger.usage()).toEqual({
      held: { frames: 1n, encodedBytes: 21n }, outstanding: { frames: 1n, encodedBytes: 21n },
      pendingConsumed: ZERO, retainedSessions: 2,
    })
  })

  it.each([
    { name: 'frame limit', capacity: { frames: 1n, encodedBytes: 1n << 20n }, size: 21 },
    { name: 'byte limit', capacity: { frames: 100n, encodedBytes: 42n }, size: 22 },
  ])('enforces the independent $name without partial accounting', ({ capacity, size }) => {
    const ledger = createLedger({ capacity })
    const session = ledger.open(sessionId())
    ledger.receive(session, 21)
    const before = ledger.usage()
    expect(() => ledger.receive(session, size)).toThrow('capacity')
    expect(ledger.usage()).toEqual(before)
  })

  it.each([
    { name: 'default capacity', capacity: { frames: 256n, encodedBytes: 16n << 20n }, size: 21 },
    { name: 'one frame', capacity: { frames: 1n, encodedBytes: 1n << 20n }, size: 21 },
    { name: 'few frames', capacity: { frames: 3n, encodedBytes: 1n << 20n }, size: 21 },
    { name: 'one large envelope', capacity: { frames: 256n, encodedBytes: BigInt(MAX_ENVELOPE_BYTES) }, size: MAX_ENVELOPE_BYTES },
  ])('returns consumed credit independently of declared capacity: $name', ({ capacity, size }) => {
    const ledger = createLedger({ capacity })
    const session = ledger.open(sessionId())
    for (let round = 1n; round <= 12n; round += 1n) {
      ledger.consume(ledger.receive(session, size))
      expect(ledger.feedbackReady()).toBe(true)
      expect(ledger.takeConsumed()).toEqual({
        relaySessionId: sessionId(), totalFrames: round, totalEncodedBytes: BigInt(size) * round,
      })
      expect(ledger.usage().outstanding).toEqual(ZERO)
    }
  })

  it('coalesces pending consumption without starving a sibling', () => {
    const ledger = createLedger({ capacity: { frames: 8n, encodedBytes: 1n << 20n } })
    const first = ledger.open(sessionId(1))
    const second = ledger.open(sessionId(2))
    ledger.consume(ledger.receive(first, 21))
    expect(ledger.feedbackReady()).toBe(true)
    ledger.consume(ledger.receive(second, 22))
    expect(ledger.feedbackReady()).toBe(true)
    expect(ledger.takeConsumed()?.relaySessionId).toEqual(sessionId(1))
    ledger.consume(ledger.receive(first, 23))
    expect(ledger.takeConsumed()?.relaySessionId).toEqual(sessionId(2))
    expect(ledger.feedbackReady()).toBe(true)
    expect(ledger.takeConsumed()).toEqual({ relaySessionId: sessionId(1), totalFrames: 2n, totalEncodedBytes: 44n })
    expect(ledger.feedbackReady()).toBe(false)
    expect(ledger.takeConsumed()).toBeUndefined()
    expect(ledger.usage()).toEqual({ held: ZERO, outstanding: ZERO, pendingConsumed: ZERO, retainedSessions: 2 })
  })

  it('returns sparse consumption without waiting for more input or a deadline', () => {
    const ledger = createLedger()
    const session = ledger.open(sessionId())
    expect(ledger.feedbackReady()).toBe(false)
    ledger.consume(ledger.receive(session, 23))
    expect(ledger.takeConsumed()).toEqual({ relaySessionId: sessionId(), totalFrames: 1n, totalEncodedBytes: 23n })
    expect(ledger.takeConsumed()).toBeUndefined()
  })

  it('requires original FIFO receipts and keeps rejected completion atomic', () => {
    const ledger = createLedger()
    const session = ledger.open(sessionId())
    const first = ledger.receive(session, 21)
    const second = ledger.receive(session, 40)
    const before = ledger.usage()
    expect(() => ledger.consume(second)).toThrow('receipt')
    expect(() => ledger.consume({ ...first } as ReceiveReceipt)).toThrow('receipt')
    expect(() => ledger.discard(first)).toThrow('retirement')
    expect(ledger.usage()).toEqual(before)
    ledger.consume(first)
    const after = ledger.usage()
    expect(() => ledger.consume(first)).toThrow('receipt')
    expect(() => ledger.discard(first)).toThrow('receipt')
    expect(ledger.usage()).toEqual(after)
    ledger.consume(second)
    expect(ledger.takeConsumed()).toEqual({ relaySessionId: sessionId(), totalFrames: 2n, totalEncodedBytes: 61n })
    expect(ledger.takeConsumed()).toBeUndefined()
  })

  it('rejects invalid configuration, admission and envelope lengths without creating state', () => {
    for (const config of [
      { connectionId: '' }, { maxSessions: 0 }, { maxSessions: 1.5 },
      { capacity: { frames: 0n, encodedBytes: 1n } },
      { capacity: { frames: MAX_UINT64 + 1n, encodedBytes: 1n } },
      { capacity: { frames: 1n, encodedBytes: -1n } },
      { capacity: { frames: 1n, encodedBytes: MAX_UINT64 + 1n } },
    ]) expect(() => createLedger(config)).toThrow('config')
    const ledger = createLedger()
    for (const id of [new Uint8Array(8), new Uint8Array(7), new Uint8Array(9)]) {
      expect(() => ledger.open(id)).toThrow('session')
    }
    expect(ledger.usage().retainedSessions).toBe(0)
    const session = ledger.open(sessionId())
    expect(() => ledger.open(sessionId())).toThrow('session')
    const before = ledger.usage()
    for (const size of [0, 20, -1, 21.5, MAX_ENVELOPE_BYTES + 1, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(() => ledger.receive(session, size)).toThrow('frame-size')
    }
    expect(ledger.usage()).toEqual(before)
    ledger.receive(session, MAX_ENVELOPE_BYTES)
    // Capacity axes are independent declarations, not possible envelope totals.
    expect(() => createLedger({ capacity: { frames: MAX_UINT64, encodedBytes: 1n } })).not.toThrow()
  })

  it('keeps cumulative arithmetic exact above Number precision and refuses u64 exhaustion', () => {
    let prefix = { frames: (1n << 53n) + 1n, encodedBytes: ((1n << 53n) + 1n) * 21n }
    prefix = appendEnvelope(prefix, 21)
    expect(prefix).toEqual({ frames: (1n << 53n) + 2n, encodedBytes: ((1n << 53n) + 2n) * 21n })
    let boundary = { frames: MAX_UINT64 / 21n - 2n, encodedBytes: MAX_UINT64 - 42n }
    boundary = appendEnvelope(appendEnvelope(boundary, 21), 21)
    const feedback = { relaySessionId: sessionId(), totalFrames: boundary.frames, totalEncodedBytes: boundary.encodedBytes }
    expect(decodeDeliveryConsumed(encodeDeliveryConsumed(feedback))).toEqual(feedback)
    expect(boundary.encodedBytes).toBe(MAX_UINT64)
    expect(() => appendEnvelope(boundary, 21)).toThrow('counter-exhausted')
    expect(() => appendEnvelope({ frames: MAX_UINT64, encodedBytes: 0n }, 21)).toThrow('counter-exhausted')
  })

  it('copies the capacity declaration instead of retaining mutable caller configuration', () => {
    const capacity = { frames: 1n, encodedBytes: 42n }
    const ledger = createLedger({ capacity })
    capacity.frames = 100n
    capacity.encodedBytes = 1n << 20n
    const session = ledger.open(sessionId())
    ledger.receive(session, 21)
    expect(() => ledger.receive(session, 21)).toThrow('capacity')
  })
})
