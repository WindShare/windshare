import { afterEach, expect, it, vi } from 'vitest'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'
import type { V2ProtocolTraceEvent, V2ProtocolTraceObserver } from '../../src/session/v2-diagnostics'
import { V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'
import { FakeSession, FakeSessionFactory, TrackedRelay, core, deferred, identity, supervisorFixture } from './v2-supervisor-fixture'

afterEach(() => vi.useRealTimers())

function fixture(laneIds = [1, 2, 3, 4]) {
  const session = new FakeSession(laneIds)
  const factory = new FakeSessionFactory()
  factory.attachRelayImpl = async (_session, signal) => new Promise((_resolve, reject) => {
    signal.addEventListener('abort', () => reject(signal.reason), { once: true })
  })
  const trace: V2ProtocolTraceEvent[] = []
  const protocolTrace: { current: V2ProtocolTraceObserver | undefined } = {
    current: event => trace.push(event),
  }
  return { session, trace, protocolTrace,
    ...supervisorFixture(session, new TrackedRelay(1), factory, undefined, undefined, protocolTrace) }
}

async function flush(): Promise<void> {
  for (let turn = 0; turn < 64; turn += 1) await Promise.resolve()
}

it('finishes a catalog request on the surviving fourth lane without replacing the session', async () => {
  const { session, factory, supervisor, trace } = fixture()
  const attempted: number[] = []
  const page = Uint8Array.of(7, 8, 9)
  Object.assign(session, { beginOperation: vi.fn(async () => {
    const laneId = session.laneIds()[0]!
    attempted.push(laneId)
    return { next: async () => {
      if (laneId !== 4) {
        const error = new V2SessionRuntimeError('lane', 'Physical lane failed')
        session.detach(laneId, error)
        throw error
      }
      return { kind: V2_MESSAGE_KIND.catalogResult, body: encodeCanonicalCbor(new Map<number, number | Uint8Array>([
        [0, 1], [1, page],
      ])) }
    } }
  }) })
  let value: Uint8Array | undefined
  const controller = new AbortController()
  const request = supervisor.catalogOperations.fetchPage(
    { directoryId: identity(2), pageIndex: 0 }, controller.signal,
  ).then(result => { value = result })
  try {
    await flush()
    expect(attempted).toEqual([1, 2, 3, 4])
    expect(value).toEqual(page)
    expect(trace).toEqual([1, 2, 3].map(revision => expect.objectContaining({
      eventName: 'operation_recovery', transition: 'retry_available_lanes',
      operationSequence: 1, generationId: 1, availabilityRevision: revision,
      laneCount: 4 - revision,
    })))
    expect(supervisor.generationId).toBe(1)
    expect(factory.connectFreshCalls).toBe(0)
  } finally {
    controller.abort()
    await request.catch(() => undefined)
    await supervisor.close()
  }
})

it('bounds retries when availability stays unchanged and preserves the original error', async () => {
  vi.useFakeTimers()
  const { supervisor, factory, trace } = fixture([1])
  const failure = new V2SessionRuntimeError('lane', 'Session writer queue is full')
  const operation = vi.fn(async () => { throw failure })
  let outcome: unknown
  const request = supervisor.execute(undefined, operation).catch(error => { outcome = error })
  try {
    await flush()
    expect(operation).toHaveBeenCalledTimes(1)
    await vi.runAllTimersAsync()
    expect(operation).toHaveBeenCalledTimes(3)
    expect(outcome).toBe(failure)
    expect(trace).toMatchObject([
      { transition: 'wait_for_availability', delayMilliseconds: 100 },
      { transition: 'wait_for_availability', delayMilliseconds: 200 },
      { transition: 'exhausted', unchangedAvailabilityRetries: 2 },
    ])
    await expect(supervisor.execute(undefined, async () => 'another request')).resolves
      .toMatchObject({ value: 'another request' })
    expect(factory.connectFreshCalls).toBe(0)
    const connection = vi.fn()
    supervisor.connection.subscribe(connection)()
    expect(connection).toHaveBeenCalledWith({ kind: 'connected' })
  } finally {
    await supervisor.close()
    await request
  }
})

it('retries temporary lane pressure after backoff without requiring a lane event', async () => {
  vi.useFakeTimers()
  const { supervisor } = fixture([1])
  const operation = vi.fn()
    .mockRejectedValueOnce(new V2SessionRuntimeError('lane', 'Queue full'))
    .mockResolvedValue('ready')
  const request = supervisor.execute(undefined, operation)
  try {
    await flush()
    expect(operation).toHaveBeenCalledTimes(1)
    await vi.runAllTimersAsync()
    await expect(request).resolves.toMatchObject({ value: 'ready' })
    expect(operation).toHaveBeenCalledTimes(2)
  } finally { await supervisor.close() }
})

it.each(['attached', 'detached', 'reattached'] as const)(
  'retries immediately when a lane is %s during backoff',
  async change => {
    vi.useFakeTimers()
    const { supervisor, session } = fixture([1, 2])
    const operation = vi.fn()
      .mockRejectedValueOnce(new V2SessionRuntimeError('lane', 'Lane unavailable'))
      .mockResolvedValue('ready')
    let value: unknown
    const request = supervisor.execute(undefined, operation).then(result => { value = result.value })
    try {
      await flush()
      expect(operation).toHaveBeenCalledTimes(1)
      if (change === 'attached') session.attach(3)
      else if (change === 'detached') session.detach(2)
      else { session.detach(2); session.attach(2) }
      await flush()
      expect(value).toBe('ready')
      expect(supervisor.generationId).toBe(1)
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      await supervisor.close()
      await request.catch(() => undefined)
    }
  },
)

it('resets the unchanged-availability retry budget after connection progress', async () => {
  vi.useFakeTimers()
  const { supervisor, session } = fixture([1, 2])
  const failure = new V2SessionRuntimeError('lane', 'Queue full')
  const operation = vi.fn()
    .mockRejectedValueOnce(failure)
    .mockRejectedValueOnce(failure)
    .mockRejectedValueOnce(failure)
    .mockResolvedValue('ready')
  let value: unknown
  const request = supervisor.execute(undefined, operation).then(result => { value = result.value })
  try {
    await flush()
    await vi.advanceTimersToNextTimerAsync()
    expect(operation).toHaveBeenCalledTimes(2)
    session.detach(2)
    await flush()
    await vi.runAllTimersAsync()
    expect(value).toBe('ready')
    expect(operation).toHaveBeenCalledTimes(4)
  } finally {
    await supervisor.close()
    await request.catch(() => undefined)
  }
})

it('waits for a replacement only when the last lane is lost', async () => {
  const { supervisor, session, factory } = fixture([1])
  const replacement = deferred<ReturnType<typeof core>>()
  factory.connectFreshImpl = () => replacement.promise
  const operation = vi.fn(async () => {
    if (supervisor.generationId === 1) {
      session.detach(1)
      throw new V2SessionRuntimeError('lane', 'Last lane failed')
    }
    return 'ready'
  })
  const request = supervisor.execute(undefined, operation)
  try {
    await flush()
    expect(operation).toHaveBeenCalledTimes(1)
    expect(factory.connectFreshCalls).toBe(1)
    replacement.resolve(core(new FakeSession([10]), new TrackedRelay(10)))
    await expect(request).resolves.toMatchObject({ value: 'ready', generation: { id: 2 } })
  } finally { await supervisor.close() }
})

it.each(['cancel', 'close', 'terminal'] as const)(
  'settles a backoff wait on %s and releases timers',
  async action => {
    vi.useFakeTimers()
    const { supervisor, session } = fixture([1, 2])
    const controller = new AbortController()
    const operation = vi.fn(async () => { throw new V2SessionRuntimeError('lane', 'Queue full') })
    const reason = new V2SessionRuntimeError('session', 'Authenticated failure')
    let error: unknown
    const request = supervisor.execute(controller.signal, operation).catch(cause => { error = cause })
    await flush()
    if (action === 'cancel') controller.abort(reason)
    else if (action === 'close') await supervisor.close()
    else session.detach(2, reason)
    await request
    expect(operation).toHaveBeenCalledTimes(1)
    if (action === 'close') expect(error).toMatchObject({ name: 'AbortError' })
    else expect(error).toBe(reason)
    expect(vi.getTimerCount()).toBe(0)
    await supervisor.close()
  },
)

it('retries on an already installed generation when an old operation fails late', async () => {
  const { supervisor, session, factory, trace } = fixture([1])
  factory.connectFreshImpl = async () => core(new FakeSession([10]), new TrackedRelay(10))
  const releaseFailure = deferred<void>()
  const operation = vi.fn(async () => {
    if (operation.mock.calls.length === 1) {
      await releaseFailure.promise
      throw new V2SessionRuntimeError('lane', 'Late old-session failure')
    }
    return 'ready'
  })
  const request = supervisor.execute(undefined, operation)
  try {
    await flush()
    session.detach(1)
    await flush()
    expect(supervisor.generationId).toBe(2)
    releaseFailure.resolve()
    await expect(request).resolves.toMatchObject({ value: 'ready', generation: { id: 2 } })
    expect(factory.connectFreshCalls).toBe(1)
    expect(trace).toMatchObject([{ transition: 'wait_for_generation', generationId: 1 }])
  } finally { await supervisor.close() }
})

it('keeps retry budgets independent and ignores a failing trace observer', async () => {
  vi.useFakeTimers()
  const { supervisor, protocolTrace } = fixture([1])
  protocolTrace.current = () => { throw new Error('Observer failed') }
  const failure = new V2SessionRuntimeError('lane', 'Queue full')
  const busy = vi.fn(async () => { throw failure })
  const ready = vi.fn().mockRejectedValueOnce(failure).mockResolvedValue('ready')
  const failed = expect(supervisor.execute(undefined, busy)).rejects.toBe(failure)
  const succeeded = expect(supervisor.execute(undefined, ready)).resolves.toMatchObject({ value: 'ready' })
  await vi.runAllTimersAsync()
  await Promise.all([failed, succeeded])
  expect(busy).toHaveBeenCalledTimes(3)
  expect(ready).toHaveBeenCalledTimes(2)
  expect(vi.getTimerCount()).toBe(0)
  await supervisor.close()
})

it('does not retry operation failures that are not connection failures', async () => {
  const { supervisor, factory } = fixture([1])
  const failure = new V2SessionRuntimeError('operation', 'Operation rejected')
  const operation = vi.fn(async () => { throw failure })
  try {
    await expect(supervisor.execute(undefined, operation)).rejects.toBe(failure)
    expect(operation).toHaveBeenCalledTimes(1)
    expect(factory.connectFreshCalls).toBe(0)
  } finally { await supervisor.close() }
})
