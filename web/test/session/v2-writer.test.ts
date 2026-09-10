import { afterEach, describe, expect, it, vi } from 'vitest'
import { V2EnvelopeSealer } from '../../src/session/v2-envelope'
import { encodeV2Body, encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { V2_SESSION_SEND_TIMEOUT_MILLISECONDS, V2SessionWriter } from '../../src/session/v2-writer'
import { BackpressuredChannel, BINDING, deferred, id, KEY, openSent } from './v2-send-fixture'

afterEach(() => vi.useRealTimers())

const message = (seed: number) => encodeV2Message(V2_MESSAGE_KIND.listChildren, id(seed), encodeV2Body([]))

describe('session writer delivery ownership', () => {
  it('withdraws queued work without consuming a sequence or closing the lane', async () => {
    const channel = new BackpressuredChannel()
    const failure = vi.fn()
    const writer = new V2SessionWriter(channel, new V2EnvelopeSealer(KEY, BINDING), { onFailure: failure })
    const first = writer.send(message(1))
    await channel.sending.promise
    const signal = new AbortController()
    const withdrawn = writer.send(message(2), { signal: signal.signal })
    const reason = new Error('cancel queued')
    signal.abort(reason)
    await expect(withdrawn).rejects.toBe(reason)
    const last = writer.send(message(3))
    channel.unblock()
    await Promise.all([first, last])
    expect((await openSent(channel)).map(value => [value.sequence, value.message.operationId![0]])).toEqual([
      [0n, 1], [1n, 3],
    ])
    expect(failure).not.toHaveBeenCalled()
    expect(channel.state).toBe('open')
  })

  it('withdraws queued followups while preserving the operation cancellation notification', async () => {
    const channel = new BackpressuredChannel()
    const writer = new V2SessionWriter(channel, new V2EnvelopeSealer(KEY, BINDING), { onFailure: () => undefined })
    const reason = new Error('operation cancelled')
    const first = expect(writer.send(message(1))).rejects.toBe(reason)
    await channel.sending.promise
    const followup = expect(writer.send(message(1))).rejects.toBe(reason)
    const cancellation = writer.send(encodeV2Message(V2_MESSAGE_KIND.cancel, id(1), encodeV2Body([1])))
    const other = writer.send(message(2))
    writer.cancelPendingMessages(id(1), reason)
    await Promise.all([first, followup])
    channel.unblock()
    await Promise.all([cancellation, other])
    expect((await openSent(channel)).map(value => value.message.kind)).toEqual([
      V2_MESSAGE_KIND.listChildren, V2_MESSAGE_KIND.cancel, V2_MESSAGE_KIND.listChildren,
    ])
  })

  it('keeps the sealing turn after cancellation and sends its frame before later work', async () => {
    const channel = new BackpressuredChannel()
    channel.unblock()
    const seal = deferred<void>()
    const sealer = new V2EnvelopeSealer(KEY, BINDING)
    let calls = 0
    const writer = new V2SessionWriter(channel, {
      seal: async plaintext => { calls += 1; await seal.promise; return sealer.seal(plaintext) },
    }, { onFailure: () => undefined })
    const first = writer.enqueue(message(1))
    const next = writer.send(message(2))
    const reason = new Error('cancel during encryption')
    expect(first.cancel(reason)).toBe('committed')
    await expect(first.completion).rejects.toBe(reason)
    expect(calls).toBe(1)
    expect(channel.sent).toHaveLength(0)
    seal.resolve()
    await next
    expect((await openSent(channel)).map(value => value.sequence)).toEqual([0n, 1n])
  })

  it('retires a stalled delivery on its own deadline after its caller leaves', async () => {
    vi.useFakeTimers()
    const channel = new BackpressuredChannel()
    const failure = vi.fn(() => { channel.close().catch(() => undefined) })
    const writer = new V2SessionWriter(channel, new V2EnvelopeSealer(KEY, BINDING), { onFailure: failure })
    const active = writer.enqueue(message(1))
    await channel.sending.promise
    const reason = new Error('cancel while sending')
    active.cancel(reason)
    await expect(active.completion).rejects.toBe(reason)
    const queued = expect(writer.send(message(2))).rejects.toMatchObject({ scope: 'lane' })
    await vi.advanceTimersByTimeAsync(V2_SESSION_SEND_TIMEOUT_MILLISECONDS)
    await queued
    expect(failure).toHaveBeenCalledOnce()
    expect(channel.state).toBe('closed')
    expect(channel.sent).toHaveLength(0)
    await expect(writer.send(message(3))).rejects.toThrow('send timed out')
  })

  it('rejects active work on close and never sends a late encryption result', async () => {
    vi.useFakeTimers()
    const channel = new BackpressuredChannel()
    channel.unblock()
    const sealed = deferred<Uint8Array<ArrayBuffer>>()
    const writer = new V2SessionWriter(channel, { seal: () => sealed.promise }, { onFailure: () => undefined })
    const active = writer.send(message(1))
    const reason = new Error('lane closed during encryption')
    writer.fail(reason)
    await expect(active).rejects.toBe(reason)
    expect(vi.getTimerCount()).toBe(0)
    sealed.resolve(new Uint8Array(1))
    await Promise.resolve()
    expect(channel.sent).toHaveLength(0)
  })

  it('retires on send failure instead of attempting a later sequence', async () => {
    const channel = new BackpressuredChannel()
    const failure = vi.fn()
    const writer = new V2SessionWriter(channel, new V2EnvelopeSealer(KEY, BINDING), { onFailure: failure })
    const first = expect(writer.send(message(1))).rejects.toThrow('Channel closed')
    await channel.sending.promise
    const second = expect(writer.send(message(2))).rejects.toThrow('Channel closed')
    await channel.close()
    await Promise.all([first, second])
    expect(failure).toHaveBeenCalledOnce()
    expect(channel.sent).toHaveLength(0)
  })
})
