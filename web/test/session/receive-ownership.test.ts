import { describe, expect, it, vi } from 'vitest'

import { ReceiveSessionPreparation } from '../../src/session'
import { TestSink, gate, transferPlan } from './helpers'

const identityOpener = {
  open: async (_index: unknown, ciphertext: Uint8Array) => ciphertext.slice(),
}
const receiveOptions = { maxBlockBytes: 16 }

describe('receive sink ownership', () => {
  it('shares one pre-transfer cleanup and preserves its primary cause', async () => {
    const sink = new TestSink()
    const cleanup = gate()
    const cancellation = new DOMException('cancel before transfer', 'AbortError')
    const cleanupFailure = new Error('prepared sink cleanup failed')
    sink.abortHook = vi.fn(async (reason) => {
      expect(reason).toBe(cancellation)
      await cleanup.promise
      throw cleanupFailure
    })
    const preparation = new ReceiveSessionPreparation(sink)
    preparation.prepare(transferPlan(0, 1), identityOpener, receiveOptions)

    const first = preparation.settleFailure(cancellation)
    const second = preparation.settleFailure(new Error('must not replace the owner cause'))
    expect(first).toBe(second)
    expect(sink.abortHook).toHaveBeenCalledOnce()
    cleanup.open()

    const failure = await first
    expect(failure).toBeInstanceOf(AggregateError)
    expect((failure as AggregateError).errors).toEqual([cancellation, cleanupFailure])
    expect((failure as AggregateError).cause).toBe(cancellation)
    expect(preparation.state).toBe('closed')
  })

  it('closes an already-cancelled transfer boundary through the session exactly once', async () => {
    const sink = new TestSink()
    sink.abortHook = vi.fn(async () => undefined)
    const cancellation = new DOMException('cancel at transfer', 'AbortError')
    const controller = new AbortController()
    controller.abort(cancellation)
    const preparation = new ReceiveSessionPreparation(sink)
    preparation.prepare(transferPlan(0, 1), identityOpener, receiveOptions)

    const started = preparation.transfer(controller.signal)
    expect(() => preparation.transfer(controller.signal)).toThrow(
      'receive sink ownership can only be transferred once',
    )
    const sessionFailure = await started.completion.catch((error: unknown) => error)
    const authoritativeFailure = await preparation.settleFailure(cancellation)

    expect(authoritativeFailure).toBe(sessionFailure)
    expect(authoritativeFailure).toBe(cancellation)
    expect(sink.abortHook).toHaveBeenCalledOnce()
    expect(sink.abortHook).toHaveBeenCalledWith(cancellation)
  })

  it('returns the session-owned post-transfer cleanup failure without aborting twice', async () => {
    const sink = new TestSink()
    const transferFailure = new Error('observer failed after transfer')
    const cleanupFailure = new Error('session sink cleanup failed')
    sink.abortHook = vi.fn(async (reason) => {
      expect(reason).toBe(transferFailure)
      throw cleanupFailure
    })
    const preparation = new ReceiveSessionPreparation(sink)
    preparation.prepare(transferPlan(0, 1), identityOpener, receiveOptions)
    const started = preparation.transfer()

    const authoritativeFailure = await preparation.settleFailure(transferFailure)
    const sessionFailure = await started.completion.catch((error: unknown) => error)

    expect(authoritativeFailure).toBe(sessionFailure)
    expect(authoritativeFailure).toBeInstanceOf(AggregateError)
    expect((authoritativeFailure as AggregateError).errors).toEqual([
      transferFailure,
      cleanupFailure,
    ])
    expect((authoritativeFailure as AggregateError).cause).toBe(transferFailure)
    expect(sink.abortHook).toHaveBeenCalledOnce()
  })
})
