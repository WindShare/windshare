import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  registerFileCheckpoint, type CheckpointClock, type CheckpointMemberInput,
} from '../../src/transfer/checkpoint/controller'

const policy = { kind: 'incremental' as const, pendingBytes: 100n, pendingMilliseconds: 1_000 }

function member(overrides: Partial<CheckpointMemberInput> = {}) {
  return {
    object: { objectId: 'actual-object', policy },
    durableBytes: 0n, pendingBytes: 1n, remainingBytes: 1_000n,
    checkpoint: vi.fn(async () => ({ kind: 'advanced' as const, durableBytes: 1n })),
    onAdvanced: vi.fn(), onFailure: vi.fn(),
    ...overrides,
  }
}

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(complete => { resolve = complete })
  return { promise, resolve }
}

describe('storage object checkpoint controller', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0) })
  afterEach(() => vi.useRealTimers())

  it('flushes already accepted unsaved data when no new writes arrive', async () => {
    const input = member({ durableBytes: 50n, checkpoint: vi.fn(async () => ({
      kind: 'advanced' as const, durableBytes: 51n,
    })) })
    const controller = registerFileCheckpoint({}, input)
    await vi.advanceTimersByTimeAsync(999)
    expect(input.checkpoint).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(input.checkpoint).toHaveBeenCalledExactlyOnceWith('pending-time')
    expect(input.onAdvanced).toHaveBeenCalledExactlyOnceWith(1n)
    expect(controller.durableBytes).toBe(51n)
    expect(vi.getTimerCount()).toBe(0)
    await controller.drain()
  })

  it('retains one unchanged deadline across frequent below-threshold writes', async () => {
    const schedule = vi.fn((callback: () => void, milliseconds: number) => {
      const handle = setTimeout(callback, milliseconds)
      return () => clearTimeout(handle)
    })
    const controller = registerFileCheckpoint({}, member({
      pendingBytes: 0n, clock: { now: () => Date.now(), schedule },
    }))
    for (let index = 0; index < 10; index += 1) {
      await controller.write(1n, async () => undefined)
      await vi.advanceTimersByTimeAsync(1)
    }
    expect(schedule).toHaveBeenCalledOnce()
    expect(vi.getTimerCount()).toBe(1)
    await controller.drain()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('uses an injected clock and coalesces accepted bytes across the shared object', async () => {
    let time = 0
    let callback: (() => void) | undefined
    const clock: CheckpointClock = {
      now: () => time,
      schedule: next => { callback = next; return () => { callback = undefined } },
    }
    const session = {}
    const first = member({ pendingBytes: 0n, clock, checkpoint: vi.fn(async () => ({
      kind: 'advanced' as const, durableBytes: 60n,
    })) })
    const second = member({ pendingBytes: 0n, clock, checkpoint: vi.fn(async () => ({
      kind: 'advanced' as const, durableBytes: 40n,
    })) })
    const a = registerFileCheckpoint(session, first)
    const b = registerFileCheckpoint(session, second)
    await a.write(60n, async () => undefined)
    expect(callback).toBeTypeOf('function')
    time = 200
    await b.write(40n, async () => undefined)
    expect(first.checkpoint).toHaveBeenCalledExactlyOnceWith('pending-bytes')
    expect(second.checkpoint).toHaveBeenCalledExactlyOnceWith('pending-bytes')
    expect(callback).toBeUndefined()
    await Promise.all([a.drain(), b.drain()])
  })

  it('evaluates an already accepted prefix-copy byte threshold without a new write or recurring timer', async () => {
    const completed = deferred()
    const input = member({
      object: { objectId: 'target', policy: { kind: 'prefix-copy', pendingBytes: 1n } },
      checkpoint: vi.fn(async () => {
        completed.resolve()
        return { kind: 'advanced' as const, durableBytes: 1n }
      }),
    })
    const controller = registerFileCheckpoint({}, input)
    await completed.promise
    await vi.advanceTimersByTimeAsync(0)
    expect(input.checkpoint).toHaveBeenCalledExactlyOnceWith('pending-bytes')
    expect(vi.getTimerCount()).toBe(0)
    await controller.drain()
  })

  it('starts a new pending interval after a sibling owning the old deadline drains', async () => {
    const session = {}
    const old = registerFileCheckpoint(session, member())
    const input = member({ pendingBytes: 0n })
    const current = registerFileCheckpoint(session, input)
    await vi.advanceTimersByTimeAsync(900)
    await old.drain()
    await vi.advanceTimersByTimeAsync(200)
    await current.write(1n, async () => undefined)
    await vi.advanceTimersByTimeAsync(999)
    expect(input.checkpoint).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(input.checkpoint).toHaveBeenCalledOnce()
    await current.drain()
  })

  it('keeps native and prefix-copy costs independent within one output session', async () => {
    const session = {}
    const native = member()
    const direct = member({ object: { objectId: 'target', policy: { kind: 'prefix-copy', pendingBytes: 100n } } })
    const a = registerFileCheckpoint(session, native)
    const b = registerFileCheckpoint(session, direct)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(native.checkpoint).toHaveBeenCalledOnce()
    expect(direct.checkpoint).not.toHaveBeenCalled()
    await Promise.all([a.drain(), b.drain()])
  })

  it('waits for the prior write and flush metadata before allowing a later write', async () => {
    const events: string[] = []
    const firstWrite = deferred()
    const flush = deferred()
    const cutStarted = deferred()
    const input = member({ pendingBytes: 0n, checkpoint: async () => {
      events.push('flush')
      cutStarted.resolve()
      await flush.promise
      events.push('metadata')
      return { kind: 'advanced', durableBytes: 1n }
    } })
    const controller = registerFileCheckpoint({}, input)
    const writing = controller.write(1n, async () => {
      events.push('first-write')
      await firstWrite.promise
      events.push('first-accepted')
    })
    await vi.advanceTimersByTimeAsync(1_000)
    expect(events).toEqual(['first-write'])
    firstWrite.resolve()
    await writing
    await vi.advanceTimersByTimeAsync(1_000)
    await cutStarted.promise
    const later = controller.write(1n, async () => { events.push('later-write') })
    expect(events).toEqual(['first-write', 'first-accepted', 'flush'])
    flush.resolve()
    await later
    expect(events).toEqual(['first-write', 'first-accepted', 'flush', 'metadata', 'later-write'])
    await controller.drain()
  })

  it('retries deferred incremental cuts after a bounded delay without waiting for network data', async () => {
    const checkpoint = vi.fn()
      .mockResolvedValueOnce({ kind: 'deferred' })
      .mockResolvedValueOnce({ kind: 'advanced', durableBytes: 1n })
    const controller = registerFileCheckpoint({}, member({ checkpoint }))
    await vi.advanceTimersByTimeAsync(1_000)
    expect(checkpoint).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(999)
    expect(checkpoint).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(checkpoint).toHaveBeenCalledTimes(2)
    expect(vi.getTimerCount()).toBe(0)
    await controller.drain()
  })

  it('paces frequent triggers by measured flush cost', async () => {
    const flush = deferred()
    const input = member({ object: { objectId: 'actual-object', policy: {
      kind: 'incremental', pendingBytes: 1n, pendingMilliseconds: 100,
    } }, checkpoint: vi.fn(async () => {
      await flush.promise
      return { kind: 'advanced' as const, durableBytes: 1n }
    }) })
    const controller = registerFileCheckpoint({}, input)
    await vi.advanceTimersByTimeAsync(0)
    await vi.advanceTimersByTimeAsync(200)
    flush.resolve()
    await vi.advanceTimersByTimeAsync(0)
    await controller.write(1n, async () => undefined)
    await vi.advanceTimersByTimeAsync(1_999)
    expect(input.checkpoint).toHaveBeenCalledOnce()
    await controller.drain()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(input.checkpoint).toHaveBeenCalledOnce()
  })

  it('cancels due work synchronously on abort and drains an active cut before settlement', async () => {
    const signal = new AbortController()
    const flush = deferred()
    const input = member({ signal: signal.signal, checkpoint: vi.fn(async () => {
      await flush.promise
      return { kind: 'advanced' as const, durableBytes: 1n }
    }) })
    const controller = registerFileCheckpoint({}, input)
    await vi.advanceTimersByTimeAsync(1_000)
    signal.abort(new Error('pause'))
    const settled = vi.fn()
    const draining = controller.drain().then(settled)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(settled).not.toHaveBeenCalled()
    expect(input.checkpoint).toHaveBeenCalledOnce()
    flush.resolve()
    await draining
    expect(settled).toHaveBeenCalledOnce()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('cancels an idle deadline before reconnect or commit and supports a new writer owner', async () => {
    const session = {}
    const input = member()
    const first = registerFileCheckpoint(session, input)
    await first.drain()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(input.checkpoint).not.toHaveBeenCalled()
    const second = registerFileCheckpoint(session, input)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(input.checkpoint).toHaveBeenCalledOnce()
    await second.drain()
  })

  it('reports a failed due cut to blocked readers and cancels later wakeups', async () => {
    const failure = new Error('metadata commit failed')
    const input = member({ checkpoint: vi.fn(async () => { throw failure }) })
    const controller = registerFileCheckpoint({}, input)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(input.onFailure).toHaveBeenCalledExactlyOnceWith(failure)
    expect(input.onAdvanced).not.toHaveBeenCalled()
    expect(input.checkpoint).toHaveBeenCalledOnce()
    await expect(controller.drain()).rejects.toBe(failure)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rejects conflicting policies for one actual object without creating another timer', async () => {
    const session = {}
    const first = registerFileCheckpoint(session, member())
    expect(() => registerFileCheckpoint(session, member({ object: {
      objectId: 'actual-object', policy: { kind: 'prefix-copy', pendingBytes: 1n },
    } }))).toThrow('conflicting cost policies')
    expect(vi.getTimerCount()).toBe(1)
    await first.drain()
  })
})
