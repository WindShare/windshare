import { describe, expect, it } from 'vitest'
import {
  decodeSettlementAck, decodeSettlementNotice, encodeSettlementAck, encodeSettlementNotice,
} from '../../../src/transport/relay/delivery-settlement-codec'
import type { ReceiveLedgerEvent } from '../../../src/contracts/relay-delivery-observation'
import type { ReceiveReceipt, ReceiveSession } from '../../../src/transport/relay/receive-ledger'
import { createLedger, notice, sessionId, ZERO } from './fixtures'

describe('receive ledger retirement settlement', () => {
  it('waits for all queued work and never reports consumption beyond a discarded hole', () => {
    const ledger = createLedger()
    const session = ledger.open(sessionId())
    const first = ledger.receive(session, 21)
    const second = ledger.receive(session, 22)
    const third = ledger.receive(session, 23)
    ledger.consume(first)
    const final = decodeSettlementNotice(encodeSettlementNotice(notice(3n, 66n)))
    ledger.notice(session, final)
    expect(ledger.takeSettlement(session)).toBeUndefined()
    expect(() => ledger.receive(session, 21)).toThrow('frozen-prefix')
    expect(() => ledger.forget(session)).toThrow('settlement-pending')
    ledger.discard(second)
    expect(ledger.takeSettlement(session)).toBeUndefined()
    ledger.consume(third)
    expect(ledger.progress(session)).toEqual({
      phase: 'draining', received: { frames: 3n, encodedBytes: 66n }, consumed: { frames: 2n, encodedBytes: 44n },
      discarded: { frames: 1n, encodedBytes: 22n }, consumedPrefix: { frames: 1n, encodedBytes: 21n },
    })
    expect(ledger.takeConsumed()).toEqual({ relaySessionId: sessionId(), totalFrames: 1n, totalEncodedBytes: 21n })
    expect(ledger.takeConsumed()).toBeUndefined()
    expect(ledger.usage().outstanding).toEqual({ frames: 2n, encodedBytes: 45n })
    const ack = ledger.takeSettlement(session)!
    expect(decodeSettlementAck(encodeSettlementAck(ack))).toEqual({ ...final, kind: 'settlement-ack' })
    expect(ledger.usage()).toEqual({ held: ZERO, outstanding: ZERO, pendingConsumed: ZERO, retainedSessions: 1 })
    ledger.forget(session)
    expect(ledger.usage().retainedSessions).toBe(0)
  })

  it('retains discarded debt and metadata until an ordered notice, including late arrivals', () => {
    const ledger = createLedger({ capacity: { frames: 2n, encodedBytes: 100n }, maxSessions: 2 })
    const retired = ledger.open(sessionId(1))
    const sibling = ledger.open(sessionId(2))
    const first = ledger.receive(retired, 21)
    ledger.retire(retired)
    ledger.retire(retired)
    ledger.discard(first)
    ledger.discard(ledger.receive(retired, 22))
    expect(ledger.progress(retired)?.phase).toBe('retiring')
    expect(ledger.usage()).toEqual({
      held: ZERO, outstanding: { frames: 2n, encodedBytes: 43n }, pendingConsumed: ZERO, retainedSessions: 2,
    })
    expect(ledger.takeSettlement(retired)).toBeUndefined()
    expect(ledger.takeConsumed()).toBeUndefined()
    expect(() => ledger.receive(sibling, 21)).toThrow('capacity')
    expect(() => ledger.open(sessionId(3))).toThrow('session-limit')
    ledger.notice(retired, notice(2n, 43n))
    expect(ledger.takeSettlement(retired)).toBeDefined()
    expect(() => ledger.open(sessionId(3))).toThrow('session-limit')
    ledger.forget(retired)
    ledger.open(sessionId(3))
    ledger.consume(ledger.receive(sibling, 23))
    expect(ledger.takeConsumed()).toEqual({ relaySessionId: sessionId(2), totalFrames: 1n, totalEncodedBytes: 23n })
  })

  it('rejects mismatched notices without retiring a live flow or changing its accounting', () => {
    const ledger = createLedger()
    const session = ledger.open(sessionId())
    ledger.receive(session, 21)
    const before = ledger.usage()
    for (const invalid of [notice(1n, 21n, 2), notice(0n, 0n), notice(2n, 42n), notice(1n, 22n)]) {
      expect(() => ledger.notice(session, invalid)).toThrow('settlement')
      expect(ledger.progress(session)?.phase).toBe('active')
      expect(ledger.usage()).toEqual(before)
      expect(ledger.takeSettlement(session)).toBeUndefined()
    }
  })

  it.each([false, true])('settles fully consumed work exactly once, with prior WS2B: %s', reported => {
    const ledger = createLedger()
    const session = ledger.open(sessionId())
    ledger.consume(ledger.receive(session, 21))
    if (reported) {
      expect(ledger.takeConsumed()).toBeDefined()
    }
    ledger.retire(session)
    expect(ledger.takeSettlement(session)).toBeUndefined()
    expect(() => ledger.forget(session)).toThrow('settlement-pending')
    ledger.notice(session, notice(1n, 21n))
    expect(ledger.takeSettlement(session)).toEqual({ ...notice(1n, 21n), kind: 'settlement-ack' })
    expect(ledger.takeConsumed()).toBeUndefined()
    expect(ledger.takeSettlement(session)).toBeUndefined()
    expect(ledger.usage()).toEqual({ held: ZERO, outstanding: ZERO, pendingConsumed: ZERO, retainedSessions: 1 })
  })

  it('repeats an acknowledgement without refunding a sibling and rejects forgotten callbacks', () => {
    const ledger = createLedger()
    const old = ledger.open(sessionId(1))
    const sibling = ledger.open(sessionId(2))
    const oldFrame = ledger.receive(old, 21)
    const siblingFrame = ledger.receive(sibling, 25)
    const final = notice(1n, 21n)
    ledger.notice(old, final)
    ledger.discard(oldFrame)
    const ack = ledger.takeSettlement(old)!
    expect(ledger.takeSettlement(old)).toBeUndefined()
    const beforeReplay = ledger.usage()
    ledger.notice(old, final)
    expect(() => ledger.forget(old)).toThrow('settlement-pending')
    expect(ledger.takeSettlement(old)).toEqual(ack)
    expect(ledger.usage()).toEqual(beforeReplay)
    ledger.forget(old)
    const replacement = ledger.open(sessionId(3))
    ledger.receive(replacement, 26)
    const beforeStale = ledger.usage()
    for (const stale of [
      () => ledger.consume(oldFrame), () => ledger.discard(oldFrame), () => ledger.retire(old),
      () => ledger.notice(old, final), () => ledger.forget(old), () => ledger.receive(old, 21),
    ]) expect(stale).toThrow('session')
    expect(ledger.takeSettlement(old)).toBeUndefined()
    expect(ledger.usage()).toEqual(beforeStale)
    ledger.consume(siblingFrame)
    expect(ledger.takeConsumed()).toEqual({ relaySessionId: sessionId(2), totalFrames: 1n, totalEncodedBytes: 25n })
  })

  it('isolates replacement connections even when the wire identity and receipt prefix match', () => {
    const oldLedger = createLedger()
    const old = oldLedger.open(sessionId())
    const receipt = oldLedger.receive(old, 21)
    oldLedger.consume(oldLedger.receive(oldLedger.open(sessionId(2)), 22))
    oldLedger.notice(old, notice(1n, 21n))
    const beforeClose = oldLedger.usage()
    oldLedger.close()
    oldLedger.close()

    const replacement = createLedger({ connectionId: 'connection-2' })
    const current = replacement.open(sessionId())
    replacement.receive(current, 21)
    const beforeStale = replacement.usage()
    expect(() => replacement.consume(receipt)).toThrow('receipt')
    expect(() => replacement.notice(old, notice(1n, 21n))).toThrow('session')
    expect(replacement.takeSettlement(old)).toBeUndefined()
    expect(replacement.usage()).toEqual(beforeStale)
    for (const stale of [
      () => oldLedger.open(sessionId(3)), () => oldLedger.receive(old, 21), () => oldLedger.consume(receipt),
      () => oldLedger.discard(receipt), () => oldLedger.retire(old), () => oldLedger.notice(old, notice(1n, 21n)),
      () => oldLedger.forget(old),
    ]) expect(stale).toThrow('closed')
    expect(oldLedger.feedbackReady()).toBe(false)
    expect(oldLedger.takeConsumed()).toBeUndefined()
    expect(oldLedger.takeSettlement(old)).toBeUndefined()
    expect(oldLedger.progress(old)).toBeUndefined()
    expect(oldLedger.usage()).toEqual(beforeClose)
  })

  it('settles an empty flow and never allocates state from unknown callbacks', () => {
    const ledger = createLedger({ maxSessions: 1 })
    expect(() => ledger.consume({} as ReceiveReceipt)).toThrow('receipt')
    expect(() => ledger.notice({} as ReceiveSession, notice(0n, 0n))).toThrow('session')
    expect(ledger.usage().retainedSessions).toBe(0)
    const session = ledger.open(sessionId())
    ledger.notice(session, notice(0n, 0n))
    expect(ledger.takeSettlement(session)).toEqual({ ...notice(0n, 0n), kind: 'settlement-ack' })
    ledger.forget(session)
    const next = ledger.open(sessionId(2))
    ledger.consume(ledger.receive(next, 21))
    expect(ledger.takeConsumed()?.relaySessionId).toEqual(sessionId(2))
  })

  it('keeps diagnostic snapshots immutable and wire IDs detached from caller-owned arrays', () => {
    const events: ReceiveLedgerEvent[] = []
    const ledger = createLedger({ observe: event => { events.push(event) } })
    const wireId = sessionId()
    const session = ledger.open(wireId)
    wireId.fill(0)
    const receipt = ledger.receive(session, 21)
    expect(() => ledger.discard(receipt)).toThrow('retirement')
    ledger.consume(receipt)
    const feedback = ledger.takeConsumed()!
    feedback.relaySessionId.fill(0)
    ledger.notice(session, notice(1n, 21n))
    const ack = ledger.takeSettlement(session)!
    ack.relaySessionId.fill(0)
    ledger.notice(session, notice(1n, 21n))
    expect(ledger.takeSettlement(session)?.relaySessionId).toEqual(sessionId())
    ledger.forget(session)
    ledger.close()

    expect(events[0]).toMatchObject({
      connectionId: 'connection-1', sessionId: '0000000000000001', action: 'open',
      progress: { phase: 'active', received: ZERO }, usage: { held: ZERO },
    })
    expect(events.find(event => event.failure)).toMatchObject({
      action: 'discard', failure: 'retirement', progress: { received: { frames: 1n, encodedBytes: 21n } },
    })
    expect(events.map(event => event.action)).toEqual([
      'open', 'discard', 'feedback', 'notice', 'settlement', 'notice', 'settlement', 'forget', 'close',
    ])
    expect(Object.isFrozen(events[0])).toBe(true)
    expect(Object.isFrozen(events[0]!.progress!.received)).toBe(true)
    expect(Object.isFrozen(events[0]!.usage)).toBe(true)
  })
})
