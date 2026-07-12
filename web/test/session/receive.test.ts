import { afterEach, describe, expect, it, vi } from 'vitest'
import { MAX_CHUNK_COUNT, createChunkIndex } from '../../src/contracts/selection'
import {
  BlockAttemptsExhaustedError,
  ReceiveSession,
  encodeBlock,
  encodeError,
} from '../../src/session'
import {
  MockFrameChannel,
  TestSink,
  gate,
  sentRequest,
  settle,
  transferPlan,
} from './helpers'

const identityOpener = {
  open: (_index: ReturnType<typeof createChunkIndex>, ciphertext: Uint8Array) =>
    ciphertext.slice(),
}

function response(index: number, payload = index): Uint8Array {
  return encodeBlock({
    index: BigInt(index),
    sequence: 0,
    last: true,
    payload: Uint8Array.of(payload),
  })
}

afterEach(() => {
  vi.useRealTimers()
})

describe('receive scheduler', () => {
  it('rejects an invalid sink delivery capability instead of silently downgrading it', () => {
    const sink = new TestSink()
    ;(sink as unknown as { deliveryOrder: string }).deliveryOrder = 'sideways'
    expect(() => new ReceiveSession(transferPlan(0, 1), sink, identityOpener, {
      maxBlockBytes: 16,
    })).toThrowError(/delivery order/u)
    expect(() => new ReceiveSession(transferPlan(0, 1), new TestSink(), identityOpener, {
      maxBlockBytes: 16,
      requestTimeoutMs: 2_147_483_648,
    })).toThrowError(/request timeout/u)
  })

  it('snapshots the validated sink delivery capability for the whole session', async () => {
    const sink = new TestSink('ascending')
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 2), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    ;(sink as unknown as { deliveryOrder: string }).deliveryOrder = 'any'
    session.addChannel(channel)
    const completed = session.start()
    await settle()
    channel.push(response(1))
    await settle()
    expect(sink.writes).toHaveLength(0)
    channel.push(response(0))
    await completed
    expect(sink.writes.map(({ index }) => index)).toEqual([0, 1])
  })

  it('does not request before explicit start and lazily subtracts sink resume state', async () => {
    const sink = new TestSink()
    sink.held.add(1)
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 3), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    session.addChannel(channel)
    await settle()
    expect(channel.sent).toHaveLength(0)

    const completed = session.start()
    await settle()
    expect(sentRequest(channel)).toEqual([0, 2])
    channel.push(response(2))
    channel.push(response(0))
    await completed
    expect(sink.writes.map(({ index }) => index)).toEqual([2, 0])
    expect(sink.finalized).toBe(true)
  })

  it('keeps maximum demand compact instead of eagerly expanding ChunkSet', async () => {
    const sink = new TestSink()
    const has = vi.spyOn(sink, 'has')
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(
      transferPlan(0, MAX_CHUNK_COUNT),
      sink,
      identityOpener,
      { maxBlockBytes: 16 },
    )
    session.addChannel(channel)
    expect(has).not.toHaveBeenCalled()
    const completion = session.start().catch((error: unknown) => error)
    await settle()
    expect(sentRequest(channel)).toEqual([0, 1, 2, 3, 4, 5, 6, 7])
    expect(has).toHaveBeenCalledTimes(8)
    await session.close()
    await completion
  })

  it('accepts out-of-order blocks while ignoring unassigned late frames', async () => {
    const sink = new TestSink()
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 3), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    session.addChannel(channel)
    const completed = session.start()
    await settle()
    channel.push(response(2, 12))
    channel.push(response(99, 99))
    channel.push(response(0, 10))
    channel.push(response(1, 11))
    await completed
    expect(sink.writes.map(({ index }) => index)).toEqual([2, 0, 1])
  })

  it('drops duplicate reassembly traffic and hot-reassigns the whole attempt', async () => {
    const sink = new TestSink()
    const first = new MockFrameChannel()
    const second = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 2), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    session.addChannel(first)
    const completed = session.start()
    await settle()
    const fragment = encodeBlock({
      index: 0n,
      sequence: 0,
      last: false,
      payload: Uint8Array.of(1),
    })
    first.push(fragment)
    first.push(fragment)
    first.push(encodeBlock({
      index: 0n,
      sequence: 1,
      last: true,
      payload: Uint8Array.of(2),
    }))
    await settle(16)

    session.addChannel(second)
    await settle()
    expect(sentRequest(second)).toEqual([0, 1])
    second.push(response(0))
    second.push(response(1))
    await completed
    expect(sink.writes.map(({ index }) => index)).toEqual([0, 1])
  })

  it('refunds a disconnected transport attempt before rejoin', async () => {
    const sink = new TestSink()
    const first = new MockFrameChannel()
    const second = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 1), sink, identityOpener, {
      maxBlockBytes: 16,
      maxBlockAttempts: 1,
    })
    session.addChannel(first)
    const completed = session.start()
    await settle()
    first.remoteClose()
    await settle()
    session.addChannel(second)
    await settle()
    expect(sentRequest(second)).toEqual([0])
    second.push(response(0))
    await completed
    expect(sink.writes).toHaveLength(1)
  })

  it('lets a hot channel progress while a sibling request send is backpressured', async () => {
    const sink = new TestSink()
    const slow = new MockFrameChannel()
    slow.sendHook = (_frame, signal) =>
      new Promise((_, reject) => {
        signal?.addEventListener('abort', () => reject(signal.reason), { once: true })
      })
    const fast = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 10), sink, identityOpener, {
      maxBlockBytes: 16,
      requestTimeoutMs: 1_000,
    })
    session.addChannel(slow)
    const completion = session.start().catch((error: unknown) => error)
    await settle()
    session.addChannel(fast)
    await settle()
    expect(sentRequest(slow)).toEqual([0, 1, 2, 3, 4, 5, 6, 7])
    expect(sentRequest(fast)).toEqual([8, 9])
    await session.close()
    await completion
  })

  it('prioritizes a stalled ordered head and bounds the reorder window', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const sink = new TestSink('ascending')
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 10), sink, identityOpener, {
      maxBlockBytes: 16,
      requestTimeoutMs: 100,
      now: () => Date.now(),
    })
    session.addChannel(channel)
    const completed = session.start()
    await settle()
    expect(sentRequest(channel)).toEqual([0, 1, 2, 3, 4, 5, 6, 7])

    for (let index = 1; index < 8; index += 1) {
      channel.push(response(index))
    }
    await settle(64)
    expect(sink.writes).toHaveLength(0)
    expect(session.snapshot().maxBufferedBlocks).toBe(7)

    await vi.advanceTimersByTimeAsync(100)
    await settle()
    expect(sentRequest(channel, 1)).toEqual([0])
    channel.push(response(0))
    await settle(32)
    expect(sentRequest(channel, 2)).toEqual([8, 9])
    channel.push(response(9))
    channel.push(response(8))
    await completed
    expect(sink.writes.map(({ index }) => index)).toEqual([0, 1, 2, 3, 4, 5, 6, 7, 8, 9])
    expect(session.snapshot().maxBufferedBlocks).toBeLessThanOrEqual(8)
  })

  it('bounds authentication retries and treats fatal peer errors as session-wide', async () => {
    const rejectingOpener = {
      open: async () => Promise.reject(new Error('authentication failed')),
    }
    const sink = new TestSink()
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 1), sink, rejectingOpener, {
      maxBlockBytes: 16,
      maxBlockAttempts: 1,
    })
    session.addChannel(channel)
    const failed = session.start()
    await settle()
    channel.push(response(0))
    await expect(failed).rejects.toBeInstanceOf(BlockAttemptsExhaustedError)
    expect(sink.abortReason).toBeInstanceOf(BlockAttemptsExhaustedError)

    const fatalSink = new TestSink()
    const fatalChannel = new MockFrameChannel()
    const fatal = new ReceiveSession(transferPlan(0, 1), fatalSink, identityOpener, {
      maxBlockBytes: 16,
    })
    fatal.addChannel(fatalChannel)
    const fatalResult = fatal.start()
    await settle()
    fatalChannel.push(encodeError(2, 'source drift'))
    await expect(fatalResult).rejects.toMatchObject({ code: 2 })
  })

  it('stops queued sink writes before aborting after the first write failure', async () => {
    const sink = new TestSink()
    const firstWrite = gate()
    const started: number[] = []
    const failure = new Error('disk write failed')
    let abortObserved = false
    sink.writeHook = async (index) => {
      started.push(index)
      if (index === 0) {
        await firstWrite.promise
        throw failure
      }
    }
    sink.abortHook = async () => {
      abortObserved = true
    }
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 3), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    session.addChannel(channel)
    const completed = session.start()
    await settle()
    channel.push(response(0))
    channel.push(response(1))
    channel.push(response(2))
    await settle(32)
    expect(started).toEqual([0])

    firstWrite.open()
    await expect(completed).rejects.toBe(failure)
    expect(started).toEqual([0])
    expect(abortObserved).toBe(true)
  })

  it('makes every concurrent close wait for sink abort settlement', async () => {
    const sink = new TestSink()
    const abort = gate()
    sink.abortHook = () => abort.promise
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 1), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    session.addChannel(channel)
    const completion = session.start().catch(() => undefined)
    await settle()

    const first = session.close(new Error('cancelled'))
    const second = session.close()
    let secondSettled = false
    second.then(() => {
      secondSettled = true
    }).catch(() => undefined)
    await settle()
    expect(secondSettled).toBe(false)

    abort.open()
    await Promise.all([first, second, completion])
    expect(sink.abortReason).toBeInstanceOf(Error)
  })

  it('preserves sink cleanup failure instead of reporting cancellation as clean', async () => {
    const sink = new TestSink()
    const cleanupFailure = new Error('exact-owned output could not be removed')
    sink.abortHook = async () => {
      throw cleanupFailure
    }
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 1), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    session.addChannel(channel)
    const abort = new AbortController()
    const completed = session.start(abort.signal)
    await settle()

    const cancellation = new DOMException('Download stopped by the user', 'AbortError')
    abort.abort(cancellation)

    const failure = await completed.catch((error: unknown) => error)
    expect(failure).toBeInstanceOf(AggregateError)
    expect((failure as AggregateError).errors).toEqual([cancellation, cleanupFailure])
    expect((failure as AggregateError).cause).toBe(cancellation)
    expect(session.state).toBe('failed')
  })

  it('reports finalizing until durable sink and channel settlement completes', async () => {
    const sink = new TestSink()
    const finalize = gate()
    sink.finalizeHook = () => finalize.promise
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(transferPlan(0, 1), sink, identityOpener, {
      maxBlockBytes: 16,
    })
    session.addChannel(channel)
    const completed = session.start()
    await settle()
    channel.push(response(0))
    await settle(32)
    expect(session.state).toBe('finalizing')

    const close = session.close()
    let closeSettled = false
    close.then(() => {
      closeSettled = true
    }).catch(() => undefined)
    await settle()
    expect(closeSettled).toBe(false)

    finalize.open()
    await Promise.all([completed, close])
    expect(session.state).toBe('completed')
    expect(sink.finalized).toBe(true)
  })
})
